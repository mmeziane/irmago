package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/go-errors/errors"
	"github.com/privacybydesign/irmago/irmaserver"
	"github.com/privacybydesign/irmago/irmaserver/server"
)

func main() {
	var err error
	defer func() {
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}()

	if len(os.Args) != 3 {
		err = errors.New("Usage: irmaserver port path")
		return
	}

	port, err := strconv.Atoi(os.Args[1])
	if err != nil {
		err = errors.New("First argument must be an integer")
		return
	}

	err = server.Start(port, &irmaserver.Configuration{
		IrmaConfigurationPath: os.Args[2],
	})
}
