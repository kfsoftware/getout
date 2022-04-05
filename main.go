package main

import (
	"github.com/kfsoftware/getout/cmd"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"os"
	"time"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	logLevel := os.Getenv("LOG_LEVEL")
	zeroLogLevel, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		zeroLogLevel = zerolog.InfoLevel
	}
	log.Logger = zerolog.New(output).With().Timestamp().Logger().Level(zeroLogLevel)
	if err := cmd.NewCmdGetOut().Execute(); err != nil {
		os.Exit(1)
	}
}
