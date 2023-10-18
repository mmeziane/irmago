package irmaserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"reflect"
	"time"

	"github.com/alexandrevicenzi/go-sse"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-errors/errors"
	"github.com/golang-jwt/jwt/v4"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/gabi/gabikeys"
	"github.com/privacybydesign/gabi/revocation"
	irma "github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/common"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

// Session helpers

func (session *sessionData) markAlive(conf *server.Configuration) {
	session.LastActive = time.Now()
	conf.Logger.
		WithFields(logrus.Fields{"session": session.RequestorToken}).
		Debug("Session marked active, deletion delayed")
}

func (session *sessionData) setStatus(status irma.ServerStatus, conf *server.Configuration) {
	conf.Logger.
		WithFields(logrus.Fields{"session": session.RequestorToken, "status": status}).
		Info("Session status updated")
	session.Status = status
	session.Result.Status = status

	// Execute callback and handler if status is Finished
	if session.Status.Finished() {
		session.doResultCallback(conf)
	}
}

func (session *sessionData) doResultCallback(conf *server.Configuration) {
	url := session.Rrequest.Base().CallbackURL
	if url == "" {
		return
	}
	server.DoResultCallback(url,
		session.Result,
		conf.JwtIssuer,
		session.Rrequest.Base().ResultJwtValidity,
		conf.JwtRSAPrivateKey,
	)
}

// Checks whether requested options are valid in the current session context.
func (session *sessionData) updateFrontendOptions(request *irma.FrontendOptionsRequest) (*irma.SessionOptions, error) {
	if session.Status != irma.ServerStatusInitialized {
		return nil, errors.New("Frontend options can only be updated when session is in initialized state")
	}
	if request.PairingMethod == "" {
		return &session.Options, nil
	} else if request.PairingMethod == irma.PairingMethodNone {
		session.Options.PairingCode = ""
	} else if request.PairingMethod == irma.PairingMethodPin {
		session.Options.PairingCode = common.NewPairingCode()
	} else {
		return nil, errors.New("Pairing method unknown")
	}
	session.Options.PairingMethod = request.PairingMethod
	return &session.Options, nil
}

// Complete the pairing process of frontend and irma client
func (session *sessionData) pairingCompleted(conf *server.Configuration) error {
	if session.Status == irma.ServerStatusPairing {
		session.setStatus(irma.ServerStatusConnected, conf)
		return nil
	}
	return errors.New("Pairing was not enabled")
}

func (session *sessionData) fail(err server.Error, message string, conf *server.Configuration) *irma.RemoteError {
	rerr := server.RemoteError(err, message)
	session.Result = &server.SessionResult{Err: rerr, Token: session.RequestorToken, Status: irma.ServerStatusCancelled, Type: session.Action}
	session.setStatus(irma.ServerStatusCancelled, conf)
	return rerr
}

func (session *sessionData) chooseProtocolVersion(minClient, maxClient *irma.ProtocolVersion) (*irma.ProtocolVersion, error) {
	minSessionProtocolVersion := minSecureProtocolVersion
	if AcceptInsecureProtocolVersions {
		// Set minimum supported version to 2.5 if condiscon compatibility is required
		minSessionProtocolVersion = minProtocolVersion
		if !session.LegacyCompatible {
			minSessionProtocolVersion = &irma.ProtocolVersion{Major: 2, Minor: 5}
		}
		// Set minimum to 2.6 if nonrevocation is required
		if len(session.Rrequest.SessionRequest().Base().Revocation) > 0 {
			minSessionProtocolVersion = &irma.ProtocolVersion{Major: 2, Minor: 6}
		}
		// Set minimum to 2.7 if chained session are used
		if session.Rrequest.Base().NextSession != nil {
			minSessionProtocolVersion = &irma.ProtocolVersion{Major: 2, Minor: 7}
		}
	}

	if minClient.AboveVersion(maxProtocolVersion) || maxClient.BelowVersion(minSessionProtocolVersion) || maxClient.BelowVersion(minClient) {
		err := errors.Errorf("Protocol version negotiation failed, min=%s max=%s minServer=%s maxServer=%s", minClient.String(), maxClient.String(), minSessionProtocolVersion.String(), maxProtocolVersion.String())
		_ = server.LogWarning(err)
		return nil, err
	}
	if maxClient.AboveVersion(maxProtocolVersion) {
		return maxProtocolVersion, nil
	} else {
		return maxClient, nil
	}
}

const retryTimeLimit = 10 * time.Second

// checkCache returns a previously cached response, for replaying against multiple requests from
// irmago's retryablehttp client, if:
// - the same body was POSTed to the same endpoint as last time
// - the body is not empty
// - last time was not more than 10 seconds ago (retryablehttp client gives up before this)
// - the session status is what it is expected to be when receiving the request for a second time.
func (session *sessionData) checkCache(endpoint string, message []byte) (int, []byte) {
	if session.ResponseCache.Endpoint != endpoint ||
		len(session.ResponseCache.Response) == 0 ||
		session.ResponseCache.SessionStatus != session.Status ||
		session.LastActive.Before(time.Now().Add(-retryTimeLimit)) ||
		sha256.Sum256(session.ResponseCache.Message) != sha256.Sum256(message) {
		session.ResponseCache = responseCache{}
		return 0, nil
	}
	return session.ResponseCache.Status, session.ResponseCache.Response
}

// Issuance helpers

func (session *sessionData) computeWitness(sk *gabikeys.PrivateKey, cred *irma.CredentialRequest, conf *server.Configuration) (*revocation.Witness, error) {
	id := cred.CredentialTypeID
	credtyp := conf.IrmaConfiguration.CredentialTypes[id]
	if !credtyp.RevocationSupported() || !session.Rrequest.SessionRequest().Base().RevocationSupported() {
		return nil, nil
	}

	// ensure the client always gets an up to date nonrevocation witness
	rs := conf.IrmaConfiguration.Revocation
	if err := rs.SyncDB(id); err != nil {
		return nil, err
	}

	// Fetch latest revocation record, and then extract the current value of the accumulator
	// from it to generate the witness from
	updates, err := rs.LatestUpdates(id, 0, &cred.KeyCounter)
	if err != nil {
		return nil, err
	}
	u := updates[cred.KeyCounter]
	if u == nil {
		return nil, errors.Errorf("no revocation updates found for key %d", cred.KeyCounter)
	}
	sig := u.SignedAccumulator
	pk, err := rs.Keys.PublicKey(id.IssuerIdentifier(), sig.PKCounter)
	if err != nil {
		return nil, err
	}
	acc, err := sig.UnmarshalVerify(pk)
	if err != nil {
		return nil, err
	}

	witness, err := revocation.RandomWitness(sk, acc)
	if err != nil {
		return nil, err
	}
	witness.SignedAccumulator = sig // attach previously selected reocation record to the witness for the client

	return witness, nil
}

func (session *sessionData) computeAttributes(
	sk *gabikeys.PrivateKey, cred *irma.CredentialRequest, conf *server.Configuration,
) ([]*big.Int, *revocation.Witness, error) {
	id := cred.CredentialTypeID
	witness, err := session.computeWitness(sk, cred, conf)
	if err != nil {
		return nil, nil, err
	}
	var nonrevAttr *big.Int
	if witness != nil {
		nonrevAttr = witness.E
	}

	issuedAt := time.Now()
	attributes, err := cred.AttributeList(conf.IrmaConfiguration, 0x03, nonrevAttr, issuedAt)
	if err != nil {
		return nil, nil, err
	}

	if witness != nil {
		issrecord := &irma.IssuanceRecord{
			CredType:   id,
			PKCounter:  &sk.Counter,
			Key:        cred.RevocationKey,
			Attr:       (*irma.RevocationAttribute)(nonrevAttr),
			Issued:     issuedAt.UnixNano(),
			ValidUntil: attributes.Expiry().UnixNano(),
		}
		err = conf.IrmaConfiguration.Revocation.SaveIssuanceRecord(id, issrecord, sk)
		if err != nil {
			return nil, nil, err
		}
	}

	return attributes.Ints, witness, nil
}

func (s *Server) validateIssuanceRequest(request *irma.IssuanceRequest) error {
	for _, cred := range request.Credentials {
		// Check that we have the appropriate private key
		iss := cred.CredentialTypeID.IssuerIdentifier()
		privatekey, err := s.conf.IrmaConfiguration.PrivateKeys.Latest(iss)
		if err != nil {
			return err
		}
		if privatekey == nil {
			return errors.Errorf("missing private key of issuer %s", iss.String())
		}
		pubkey, err := s.conf.IrmaConfiguration.PublicKey(iss, privatekey.Counter)
		if err != nil {
			return err
		}
		if pubkey == nil {
			return errors.Errorf("missing public key of issuer %s", iss.String())
		}
		now := time.Now()
		if now.Unix() > pubkey.ExpiryDate {
			return errors.Errorf("cannot issue using expired public key %s-%d", iss.String(), privatekey.Counter)
		}
		cred.KeyCounter = privatekey.Counter

		if s.conf.IrmaConfiguration.CredentialTypes[cred.CredentialTypeID].RevocationSupported() {
			settings := s.conf.RevocationSettings[cred.CredentialTypeID]
			if settings == nil || (settings.RevocationServerURL == "" && !settings.Server) {
				return errors.Errorf("revocation enabled for %s but no revocation server configured", cred.CredentialTypeID)
			}
			if cred.RevocationKey == "" {
				return errors.Errorf("revocation enabled for %s but no revocationKey specified", cred.CredentialTypeID)
			}
		}

		// Check that the credential is consistent with irma_configuration
		if err := cred.Validate(s.conf.IrmaConfiguration); err != nil {
			return err
		}

		// Ensure the credential has an expiry date
		defaultValidity := irma.Timestamp(time.Now().AddDate(0, 6, 0))
		if cred.Validity == nil {
			cred.Validity = &defaultValidity
		}
		if !AllowIssuingExpiredCredentials && cred.Validity.Before(irma.Timestamp(now)) {
			return errors.New("cannot issue expired credentials")
		}
	}

	return nil
}

func (session *sessionData) getProofP(commitments *irma.IssueCommitmentMessage, scheme irma.SchemeManagerIdentifier, conf *server.Configuration) (*gabi.ProofP, error) {
	if session.KssProofs == nil {
		session.KssProofs = make(map[irma.SchemeManagerIdentifier]*gabi.ProofP)
	}

	if _, contains := session.KssProofs[scheme]; !contains {
		str, contains := commitments.ProofPjwts[scheme.Name()]
		if !contains {
			return nil, errors.Errorf("no keyshare proof included for scheme %s", scheme.Name())
		}
		conf.Logger.Debug("Parsing keyshare ProofP JWT: ", str)
		claims := &struct {
			jwt.StandardClaims
			ProofP *gabi.ProofP
		}{}
		token, err := jwt.ParseWithClaims(str, claims, conf.IrmaConfiguration.KeyshareServerKeyFunc(scheme))
		if err != nil {
			return nil, err
		}
		if !token.Valid {
			return nil, errors.Errorf("invalid keyshare proof included for scheme %s", scheme.Name())
		}
		session.KssProofs[scheme] = claims.ProofP
	}

	return session.KssProofs[scheme], nil
}

func (session *sessionData) getClientRequest() (*irma.ClientSessionRequest, error) {
	info := irma.ClientSessionRequest{
		LDContext:       irma.LDContextClientSessionRequest,
		ProtocolVersion: session.Version,
		Options:         &session.Options,
	}

	if session.Options.PairingMethod == irma.PairingMethodNone {
		request, err := session.getRequest()
		if err != nil {
			return nil, err
		}
		info.Request = request
	}
	return &info, nil
}

func (session *sessionData) getRequest() (irma.SessionRequest, error) {
	req := session.Rrequest.SessionRequest()
	// In case of issuance requests, strip revocation keys from []CredentialRequest
	isreq, issuing := req.(*irma.IssuanceRequest)
	if !issuing {
		return req, nil
	}
	cpy, err := copyObject(isreq)
	if err != nil {
		return nil, err
	}
	for _, cred := range cpy.(*irma.IssuanceRequest).Credentials {
		cred.RevocationSupported = cred.RevocationKey != ""
		cred.RevocationKey = ""
	}
	return cpy.(*irma.IssuanceRequest), nil
}

func (session *sessionData) hash() [32]byte {
	// Note: This marshalling does not consider the order of the `map[irma.SchemeManagerIdentifier]*gabi.ProofP` items.
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		panic(err)
	}

	return sha256.Sum256(sessionJSON)
}

func (session *sessionData) timeout(conf *server.Configuration) time.Duration {
	maxSessionDuration := time.Duration(conf.MaxSessionLifetime) * time.Minute
	if session.Status == irma.ServerStatusInitialized && session.Rrequest.Base().ClientTimeout != 0 {
		maxSessionDuration = time.Duration(session.Rrequest.Base().ClientTimeout) * time.Second
	} else if session.Status.Finished() {
		maxSessionDuration = 0
	}
	return maxSessionDuration - time.Since(session.LastActive)
}

func (session *sessionData) ttl(conf *server.Configuration) time.Duration {
	return session.timeout(conf) + time.Duration(conf.SessionResultLifetime)*time.Minute
}

// UnmarshalJSON unmarshals sessionData.
func (session *sessionData) UnmarshalJSON(data []byte) error {
	type rawSession sessionData

	var temp struct {
		Rrequest json.RawMessage `json:",omitempty"`
		rawSession
	}

	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}

	if len(temp.Rrequest) == 0 {
		return errors.Errorf("temp.Rrequest == nil: %d \n", temp.Rrequest)
	}

	*session = sessionData(temp.rawSession)

	// unmarshal Rrequest
	switch session.Action {
	case "issuing":
		session.Rrequest = &irma.IdentityProviderRequest{}
	case "disclosing":
		session.Rrequest = &irma.ServiceProviderRequest{}
	case "signing":
		session.Rrequest = &irma.SignatureRequestorRequest{}
	}

	return json.Unmarshal(temp.Rrequest, session.Rrequest)
}

// Other

func (s *Server) validateRequest(request irma.SessionRequest) error {
	if _, err := s.conf.IrmaConfiguration.Download(request); err != nil {
		return err
	}
	base := request.Base()
	if err := base.Validate(s.conf.IrmaConfiguration); err != nil {
		return err
	}
	if base.AugmentReturnURL {
		if !s.conf.AugmentClientReturnURL {
			return errors.New("augmenting client return url not enabled in server configuration")
		}
		if base.ClientReturnURL == "" {
			return errors.New("cannot augment empty client return url")
		}
	}
	return request.Disclosure().Disclose.Validate(s.conf.IrmaConfiguration)
}

func copyObject(i interface{}) (interface{}, error) {
	cpy := reflect.New(reflect.TypeOf(i).Elem()).Interface()
	bts, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(bts, cpy); err != nil {
		return nil, err
	}
	return cpy, nil
}

// purgeRequest logs the request excluding any attribute values.
func purgeRequest(request irma.RequestorRequest) irma.RequestorRequest {
	// We want to log as much as possible of the request, but no attribute values.
	// We cannot just remove them from the request parameter as that would break the calling code.
	// So we create a deep copy of the request from which we can then safely remove whatever we want to.
	// Ugly hack alert: the easiest way to do this seems to be to convert it to JSON and then back.
	// As we do not know the precise type of request, we use reflection to create a new instance
	// of the same type as request, into which we then unmarshal our copy.
	cpy, err := copyObject(request)
	if err != nil {
		panic(err)
	}

	// Remove required attribute values from any attributes to be disclosed
	_ = cpy.(irma.RequestorRequest).SessionRequest().Disclosure().Disclose.Iterate(
		func(attr *irma.AttributeRequest) error {
			attr.Value = nil
			return nil
		},
	)

	// Remove attribute values from attributes to be issued
	if isreq, ok := cpy.(*irma.IdentityProviderRequest); ok {
		for _, cred := range isreq.Request.Credentials {
			cred.Attributes = nil
		}
	}

	return cpy.(irma.RequestorRequest)
}

func eventServer(conf *server.Configuration) *sse.Server {
	return sse.NewServer(&sse.Options{
		ChannelNameFunc: func(r *http.Request) string {
			ssectx := r.Context().Value("sse")
			if ssectx == nil {
				return ""
			}
			switch ssectx.(common.SSECtx).Component {
			case server.ComponentSession:
				return "session/" + ssectx.(common.SSECtx).Arg
			case server.ComponentFrontendSession:
				return "frontendsession/" + ssectx.(common.SSECtx).Arg
			case server.ComponentRevocation:
				return "revocation/" + ssectx.(common.SSECtx).Arg
			default:
				return ""
			}
		},
		Headers: map[string]string{
			"Access-Control-Allow-Origin":  "*",
			"Access-Control-Allow-Methods": "GET, OPTIONS",
			"Access-Control-Allow-Headers": "Keep-Alive,X-Requested-With,Cache-Control,Content-Type,Last-Event-ID",
		},
		Logger: log.New(conf.Logger.WithField("type", "sse").WriterLevel(logrus.DebugLevel), "", 0),
	})
}

func errorWriter(err *irma.RemoteError, writer func(w http.ResponseWriter, object interface{}, rerr *irma.RemoteError)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer(w, nil, err)
	}
}

func (s *Server) frontendMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Context().Value("session").(*sessionData)
		frontendAuth := irma.FrontendAuthorization(r.Header.Get(irma.AuthorizationHeader))

		if frontendAuth != session.FrontendAuth {
			server.WriteError(w, server.ErrorIrmaUnauthorized, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Context().Value("session").(*sessionData)

		// Read r.Body, and then replace with a fresh ReadCloser for the next handler
		message, err := io.ReadAll(r.Body)
		if err != nil {
			message = []byte("<failed to read body: " + err.Error() + ">")
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewBuffer(message))

		// if a cache is set and applicable, return it
		status, output := session.checkCache(r.URL.Path, message)
		if status > 0 && len(output) > 0 {
			w.WriteHeader(status)
			_, _ = w.Write(output)
			return
		}

		// no cache set; perform request and record output
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		buf := new(bytes.Buffer)
		ww.Tee(buf)
		next.ServeHTTP(ww, r)

		session.ResponseCache = responseCache{
			Endpoint:      r.URL.Path,
			Message:       message,
			Response:      buf.Bytes(),
			Status:        ww.Status(),
			SessionStatus: session.Status,
		}
	})
}

func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := irma.ParseClientToken(chi.URLParam(r, "clientToken"))
		if err != nil {
			server.WriteError(w, server.ErrorInvalidRequest, err.Error())
			return
		}

		if err := s.sessions.clientTransaction(token, func(session *sessionData) error {
			expectedHost := session.Rrequest.SessionRequest().Base().Host
			if expectedHost != "" && expectedHost != r.Host {
				server.WriteError(w, server.ErrorUnauthorized, "Host mismatch")
				return nil
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "session", session)))

			// Write session result to context for irmac.go functions.
			result := session.Result
			resultValue := r.Context().Value("sessionresult")
			if resultValue != nil {
				*resultValue.(*server.SessionResult) = *result
			}

			return nil
		}); err != nil {
			if _, ok := err.(*UnknownSessionError); ok {
				server.WriteError(w, server.ErrorSessionUnknown, "")
			} else {
				server.WriteError(w, server.ErrorInternal, "")
			}
			return
		}
	})
}

func (s *Server) pairingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Context().Value("session").(*sessionData)

		if session.Status == irma.ServerStatusPairing {
			server.WriteError(w, server.ErrorPairingRequired, "")
			return
		}

		// Endpoints behind the pairingMiddleware can only be accessed when the client is already connected
		// and the request includes the right authorization header to prove we still talk to the same client as before.
		if session.Status != irma.ServerStatusConnected {
			server.WriteError(w, server.ErrorUnexpectedRequest, "Session not yet started or already finished")
			return
		}
		clientAuth := irma.ClientAuthorization(r.Header.Get(irma.AuthorizationHeader))
		if session.ClientAuth != clientAuth {
			server.WriteError(w, server.ErrorIrmaUnauthorized, "")
			return
		}

		next.ServeHTTP(w, r)
	})
}
