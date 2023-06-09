package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/caarlos0/env"
	"github.com/joho/godotenv"
	"github.com/m3co/kafko"
	"github.com/m3co/kafko/log"
	"github.com/segmentio/kafka-go"
)

const (
	maxBytes = 2 << 21
)

type Config struct {
	Name         string   `env:"NAME" envDefault:"name"`
	KafkaUser    string   `env:"KAFKA_USER"`
	KafkaPass    string   `env:"KAFKA_PASS"`
	KafkaTopic   string   `env:"KAFKA_TOPIC,required"`
	KafkaBrokers []string `env:"KAFKA_BROKERS,required"`
}

func main() {
	log := log.NewLogger()
	cfg := loadConfig(log)
	shutdown := make(chan os.Signal, 1)

	signal.Notify(shutdown, syscall.SIGTERM, syscall.SIGINT)

	opts := kafko.NewOptionsListener().WithReaderFactory(func() kafko.Reader {
		return kafka.NewReader(kafka.ReaderConfig{
			GroupID:     cfg.Name,
			Topic:       cfg.KafkaTopic,
			Brokers:     cfg.KafkaBrokers,
			Dialer:      kafko.NewDialer(cfg.KafkaUser, cfg.KafkaPass),
			ErrorLogger: log,
			//Logger:   log,
			MaxBytes: maxBytes,
		})
	})

	consumer := kafko.NewListener(log, opts)

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := consumer.Listen(ctx); err != nil {
			log.Panicf(err, "err := consumer.Listen(context.Background())")
		}
	}()

	go func() {
		<-shutdown

		defer close(shutdown)

		log.Printf("shutting down")

		if err := consumer.Shutdown(context.Background()); err != nil {
			log.Errorf(err, "err := consumer.Shutdown(context.Background())")
		}
	}()

	msgChan, errChan := consumer.MessageAndErrorChannels()

	for msg := range msgChan {
		fmt.Printf("msg: %s", string(msg)) //nolint:forbidigo

		errChan <- nil
	}

	<-shutdown

	log.Printf("bye")
}

func loadConfig(log log.Logger) Config {
	if err := godotenv.Load(); err != nil {
		log.Panicf(err, "err := godotenv.Load()")
	}

	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		log.Panicf(err, "err := env.Parse(&cfg)")
	}

	return cfg
}
