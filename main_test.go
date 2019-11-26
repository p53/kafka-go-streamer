package main

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jinzhu/copier"
	"github.com/kelseyhightower/envconfig"
	kafka "github.com/segmentio/kafka-go"
	yaml "gopkg.in/yaml.v2"
)

type TestMessage struct {
	Message       string `yaml:"message"`
	ExpectedTopic string `yaml:"expected_topic"`
}

type TestSplit struct {
	InputTopic  string
	Extractor   Extractor `yaml:"extractor"`
	OutputTopic string    `yaml:"output_topic"`
	Action      string    `yaml:"action"`
}

type TestSpliter struct {
	TestSplits   []TestSplit       `yaml:"splits"`
	InputTopic   string            `yaml:"input_topic"`
	Actions      map[string]string `yaml:"actions"`
	TestMessages []TestMessage     `yaml:"test_messages"`
}

type TestSpliterCollection struct {
	Spliters []TestSpliter `yaml:"spliters_templates"`
}

func TestMessageRouting(t *testing.T) {
	spliters := TestSpliterCollection{}
	splitConf := os.Getenv("SPLIT_CONF")
	logger := GetLogger()

	data, err := base64.StdEncoding.DecodeString(splitConf)

	if err != nil {
		logger.Printf("Error: %s", err)
		return
	}

	err = yaml.Unmarshal([]byte(data), &spliters)

	if err != nil {
		logger.Fatalf("Problem with file %s: %s", splitConf, err)
	}

	templateReaderConfig := ReaderConfig{}
	templateWriterConfig := WriterConfig{}

	err = envconfig.Process("pannet-kafka-streamer", &templateReaderConfig)

	if err != nil {
		logger.Fatal(err.Error())
	}

	err = envconfig.Process("pannet-kafka-streamer", &templateWriterConfig)

	if err != nil {
		logger.Fatal(err.Error())
	}

	for _, spliter := range spliters.Spliters {
		for _, testMsg := range spliter.TestMessages {
			dialer := &kafka.Dialer{
				Timeout: 10 * time.Second,
			}

			readerConfig := templateReaderConfig
			readerConfig.Topic = testMsg.ExpectedTopic
			readerConfig.GroupID = spliter.InputTopic
			readerConfig.Dialer = dialer
			readerConfig.ErrorLogger = logger

			writerConfig := templateWriterConfig
			writerConfig.Topic = spliter.InputTopic
			writerConfig.Dialer = dialer
			writerConfig.ErrorLogger = logger

			writerKafkaConfig := &kafka.WriterConfig{}
			readerKafkaConfig := &kafka.ReaderConfig{}
			copier.Copy(&writerKafkaConfig, &writerConfig)
			copier.Copy(&readerKafkaConfig, &readerConfig)

			for index := 0; index < 20; index++ {
				conn, err := (&kafka.Dialer{
					Resolver: &net.Resolver{},
				}).DialLeader(context.Background(), "tcp", writerConfig.Brokers[0], spliter.InputTopic, 0)

				if err != nil {
					logger.Printf("Failed to open a new kafka connection: %s", err)
					defer conn.Close()
					time.Sleep(3 * time.Second)
				} else {
					break
				}
			}

			w := kafka.NewWriter(*writerKafkaConfig)
			defer w.Close()

			newMsg := kafka.Message{
				Value: []byte(testMsg.Message),
			}

			logger.Printf("%v", newMsg)

			err = w.WriteMessages(context.Background(),
				newMsg,
			)

			if err != nil {
				logger.Fatalf("%s", err)
			}

			r := kafka.NewReader(*readerKafkaConfig)
			defer r.Close()

			var m kafka.Message

			for index := 0; index < 10; index++ {
				logger.Print("Fetching")
				m, err = r.ReadMessage(context.Background())
				logger.Printf("Read message %s", m.Value)

				if err != nil {
					logger.Fatalf("%s", err)
				}

				if string(m.Value) != testMsg.Message {
					logger.Printf("Message: %s is not in expected topic: %s! %s aaa", testMsg.Message, testMsg.ExpectedTopic, m.Value)
					time.Sleep(3 * time.Second)
				} else {
					break
				}
			}

			if string(m.Value) != testMsg.Message {
				t.Fatalf("Message: %s is not in expected topic: %s!", testMsg.Message, testMsg.ExpectedTopic)
			}
		}
	}
}
