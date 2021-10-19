package irmaserver

import (
	"context"
	"encoding/json"
	"github.com/go-errors/errors"
	"strings"
	"sync"
	"time"

	"github.com/bsm/redislock"

	"github.com/alexandrevicenzi/go-sse"
	"github.com/go-redis/redis/v8"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	irma "github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/common"
	"github.com/privacybydesign/irmago/server"

	"github.com/sirupsen/logrus"
)

type session struct {
	sync.Mutex     `json:-`
	sse            *sse.Server
	locked         bool
	lock           *redislock.Lock
	sessions       sessionStore
	conf           *server.Configuration
	request        irma.SessionRequest
	statusChannels []chan irma.ServerStatus

	sessionData
}

type sessionData struct {
	Action             irma.Action
	RequestorToken     irma.RequestorToken
	ClientToken        irma.ClientToken
	Version            *irma.ProtocolVersion `json:",omitempty"`
	Rrequest           irma.RequestorRequest
	LegacyCompatible   bool // if the request is convertible to pre-condiscon format
	Status             irma.ServerStatus
	PrevStatus         irma.ServerStatus
	ResponseCache      responseCache
	LastActive         time.Time
	Result             *server.SessionResult
	KssProofs          map[irma.SchemeManagerIdentifier]*gabi.ProofP
	Next               *irma.Qr
	FrontendAuth       irma.FrontendAuthorization
	ImplicitDisclosure irma.AttributeConDisCon
	Options            irma.SessionOptions
	ClientAuth         irma.ClientAuthorization
}

type responseCache struct {
	Endpoint      string
	Message       []byte
	Response      []byte
	Status        int
	SessionStatus irma.ServerStatus
}

type sessionStore interface {
	get(token irma.RequestorToken) (*session, error)
	clientGet(token irma.ClientToken) (*session, error)
	add(session *session) error
	update(session *session) error
	lock(session *session) error
	unlock(session *session) error
	stop()
}

type memorySessionStore struct {
	sync.RWMutex
	conf *server.Configuration

	requestor map[irma.RequestorToken]*session
	client    map[irma.ClientToken]*session
}

type redisSessionStore struct {
	client *redis.Client
	locker *redislock.Client
	conf   *server.Configuration
}

type RedisError interface {
	Error() string
}

type UnknownSessionError interface {
	Error() string
}

const (
	maxSessionLifetime         = 5 * time.Minute        // After this a session is cancelled
	maxLockLifetime            = 500 * time.Millisecond // After this the Redis lock self-deletes, preventing a deadlock
	minLockRetryTime           = 30 * time.Millisecond
	maxLockRetryTime           = 2 * time.Second
	requestorTokenLookupPrefix = "token:"
	clientTokenLookupPrefix    = "session:"
	lockPrefix                 = "lock:"
)

var (
	minProtocolVersion = irma.NewVersion(2, 4)
	maxProtocolVersion = irma.NewVersion(2, 8)

	minFrontendProtocolVersion = irma.NewVersion(1, 0)
	maxFrontendProtocolVersion = irma.NewVersion(1, 1)

	lockingRetryOptions = &redislock.Options{RetryStrategy: redislock.ExponentialBackoff(minLockRetryTime, maxLockRetryTime)}
)

func (s *memorySessionStore) get(t irma.RequestorToken) (*session, error) {
	s.RLock()
	defer s.RUnlock()
	return s.requestor[t], nil
}

func (s *memorySessionStore) clientGet(t irma.ClientToken) (*session, error) {
	s.RLock()
	defer s.RUnlock()
	return s.client[t], nil
}

func (s *memorySessionStore) add(session *session) error {
	s.Lock()
	defer s.Unlock()
	s.requestor[session.RequestorToken] = session
	s.client[session.ClientToken] = session
	return nil
}

func (s *memorySessionStore) update(_ *session) error {
	return nil
}

func (s *memorySessionStore) lock(session *session) error {
	session.Lock()
	session.locked = true

	return nil
}

func (s *memorySessionStore) unlock(session *session) error {
	session.locked = false
	session.Unlock()
	return nil
}

func (s *memorySessionStore) stop() {
	s.Lock()
	defer s.Unlock()
	for _, session := range s.requestor {
		if session.sse != nil {
			session.sse.CloseChannel("session/" + string(session.RequestorToken))
			session.sse.CloseChannel("session/" + string(session.ClientToken))
			session.sse.CloseChannel("frontendsession/" + string(session.ClientToken))
		}
	}
}

func (s *memorySessionStore) deleteExpired() {
	// First check which sessions have expired
	// We don't need a write lock for this yet, so postpone that for actual deleting
	s.RLock()
	toCheck := make(map[irma.RequestorToken]*session, len(s.requestor))
	for token, session := range s.requestor {
		toCheck[token] = session
	}
	s.RUnlock()

	expired := make([]irma.RequestorToken, 0, len(toCheck))
	for token, session := range toCheck {
		session.Lock()

		timeout := maxSessionLifetime
		if session.Status == irma.ServerStatusInitialized && session.Rrequest.Base().ClientTimeout != 0 {
			timeout = time.Duration(session.Rrequest.Base().ClientTimeout) * time.Second
		}

		if session.LastActive.Add(timeout).Before(time.Now()) {
			if !session.Status.Finished() {
				s.conf.Logger.WithFields(logrus.Fields{"session": session.RequestorToken}).Infof("Session expired")
				session.markAlive()
				session.setStatus(irma.ServerStatusTimeout)
			} else {
				s.conf.Logger.WithFields(logrus.Fields{"session": session.RequestorToken}).Infof("Deleting session")
				expired = append(expired, token)
			}
		}
		session.Unlock()
	}

	// Using a write lock, delete the expired sessions
	s.Lock()
	for _, token := range expired {
		session := s.requestor[token]
		if session.sse != nil {
			session.sse.CloseChannel("session/" + string(session.RequestorToken))
			session.sse.CloseChannel("session/" + string(session.ClientToken))
			session.sse.CloseChannel("frontendsession/" + string(session.ClientToken))
		}
		delete(s.client, session.ClientToken)
		delete(s.requestor, token)
	}
	s.Unlock()
}

func (s *redisSessionStore) get(t irma.RequestorToken) (*session, error) {
	//TODO: input validation string?
	val, err := s.client.Get(context.Background(), requestorTokenLookupPrefix+string(t)).Result()
	if err == redis.Nil {
		return nil, nil
	} else if err != nil {
		return nil, logAsRedisError(err)
	}

	return s.clientGet(irma.ClientToken(val))
}

func (s *redisSessionStore) clientGet(t irma.ClientToken) (*session, error) {
	val, err := s.client.Get(context.Background(), clientTokenLookupPrefix+string(t)).Result()
	if err == redis.Nil {
		return nil, nil
	} else if err != nil {
		return nil, logAsRedisError(err)
	}

	var session session
	session.conf = s.conf
	session.sessions = s
	if err := json.Unmarshal([]byte(val), &session.sessionData); err != nil {
		return nil, logAsRedisError(err)
	}
	session.request = session.Rrequest.SessionRequest()

	if session.LastActive.Add(maxSessionLifetime).Before(time.Now()) && !session.Status.Finished() {
		s.conf.Logger.WithFields(logrus.Fields{"session": session.RequestorToken}).Infof("Session expired")
		session.markAlive()
		session.setStatus(irma.ServerStatusTimeout)
	}

	return &session, nil
}

func (s *redisSessionStore) add(session *session) error {
	timeout := 2 * maxSessionLifetime // logic similar to memory store
	if session.Status == irma.ServerStatusInitialized && session.Rrequest.Base().ClientTimeout != 0 {
		timeout = time.Duration(session.Rrequest.Base().ClientTimeout) * time.Second
	} else if session.Status.Finished() {
		timeout = maxSessionLifetime
	}

	sessionJSON, err := json.Marshal(session.sessionData)
	if err != nil {
		return logAsRedisError(err)
	}

	err = s.client.Set(context.Background(), requestorTokenLookupPrefix+string(session.sessionData.RequestorToken), string(session.sessionData.ClientToken), timeout).Err()
	if err != nil {
		return logAsRedisError(err)
	}
	err = s.client.Set(context.Background(), clientTokenLookupPrefix+string(session.sessionData.ClientToken), sessionJSON, timeout).Err()
	if err != nil {
		return logAsRedisError(err)
	}

	return nil
}

func (s *redisSessionStore) update(session *session) error {
	// Time passes between acquiring the lock and writing to Redis. Check before write action that lock is still valid.
	if session.lock == nil {
		return errors.New("Session lock is not set.")
	} else if ttl, err := session.lock.TTL(context.Background()); err != nil {
		return errors.New("Lock cannot be checked.")
	} else if ttl == 0 {
		return errors.New("No session lock available.")
	}
	return s.add(session)
}

func (s *redisSessionStore) lock(session *session) error {
	lock, err := s.locker.Obtain(context.Background(), lockPrefix+string(session.ClientToken), maxLockLifetime, lockingRetryOptions)
	if err == redislock.ErrNotObtained {
		return server.LogWarning(RedisError(err))
	} else if err != nil {
		return logAsRedisError(err)
	}
	session.locked = true
	session.lock = lock

	return nil
}

func (s *redisSessionStore) unlock(session *session) error {
	err := session.lock.Release(context.Background())
	if err == redislock.ErrLockNotHeld {
		return server.LogWarning(RedisError(err))
	} else if err != nil {
		return logAsRedisError(err)
	}
	return nil
}

func (s *redisSessionStore) stop() {
	err := s.client.Close()
	if err != nil {
		_ = logAsRedisError(err)
	}
}

var one *big.Int = big.NewInt(1)

func (s *Server) newSession(action irma.Action, request irma.RequestorRequest, disclosed irma.AttributeConDisCon, FrontendAuth irma.FrontendAuthorization) (*session, error) {
	clientToken := irma.ClientToken(common.NewSessionToken())
	requestorToken := irma.RequestorToken(common.NewSessionToken())
	if len(FrontendAuth) == 0 {
		FrontendAuth = irma.FrontendAuthorization(common.NewSessionToken())
	}

	base := request.SessionRequest().Base()
	if s.conf.AugmentClientReturnURL && base.AugmentReturnURL && base.ClientReturnURL != "" {
		if strings.Contains(base.ClientReturnURL, "?") {
			base.ClientReturnURL += "&token=" + string(requestorToken)
		} else {
			base.ClientReturnURL += "?token=" + string(requestorToken)
		}
	}

	sd := sessionData{
		Action:         action,
		Rrequest:       request,
		LastActive:     time.Now(),
		RequestorToken: requestorToken,
		ClientToken:    clientToken,
		Status:         irma.ServerStatusInitialized,
		PrevStatus:     irma.ServerStatusInitialized,
		Result: &server.SessionResult{
			LegacySession: request.SessionRequest().Base().Legacy(),
			Token:         requestorToken,
			Type:          action,
			Status:        irma.ServerStatusInitialized,
		},
		Options: irma.SessionOptions{
			LDContext:     irma.LDContextSessionOptions,
			PairingMethod: irma.PairingMethodNone,
		},
		FrontendAuth:       FrontendAuth,
		ImplicitDisclosure: disclosed,
	}
	ses := &session{
		sessionData: sd,
		sessions:    s.sessions,
		sse:         s.serverSentEvents,
		conf:        s.conf,
		request:     request.SessionRequest(),
	}

	s.conf.Logger.WithFields(logrus.Fields{"session": ses.RequestorToken}).Debug("New session started")
	nonce, _ := gabi.GenerateNonce()
	base.Nonce = nonce
	base.Context = one

	// lock session
	_ = s.sessions.lock(ses)
	err := s.sessions.add(ses)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.sessions.unlock(ses) }()

	return ses, nil
}

func logAsRedisError(err error) error {
	return server.LogError(RedisError(err))
}
