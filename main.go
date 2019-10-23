package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crypto/tls"
	"crypto/x509"

	"github.com/jinzhu/copier"
	"github.com/kelseyhightower/envconfig"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

var logger *zap.Logger

type Error struct {
	message string
}

func (e Error) Error() string {
	return e.message
}

type ReaderConfig struct {
	Brokers                []string `envconfig:"broker_list" required:"true"`
	GroupID                string
	Topic                  string
	Partition              int
	Dialer                 *kafka.Dialer
	QueueCapacity          int           `envconfig:"reader_queue_capacity"`
	MinBytes               int           `envconfig:"reader_min_bytes"`
	MaxBytes               int           `envconfig:"reader_max_bytes"`
	MaxWait                time.Duration `envconfig:"reader_max_wait"`
	ReadLagInterval        time.Duration `envconfig:"reader_read_lag_interval"`
	GroupBalancers         []kafka.GroupBalancer
	HeartbeatInterval      time.Duration `envconfig:"reader_heartbeat_interval"`
	CommitInterval         time.Duration `envconfig:"reader_commit_interval"`
	PartitionWatchInterval time.Duration `envconfig:"reader_partition_watch_interval"`
	WatchPartitionChanges  bool          `envconfig:"reader_watch_partition_changes"`
	SessionTimeout         time.Duration `envconfig:"reader_session_timeout"`
	RebalanceTimeout       time.Duration `envconfig:"reader_rebalance_timeout"`
	JoinGroupBackoff       time.Duration `envconfig:"reader_join_group_backoff"`
	RetentionTime          time.Duration `envconfig:"reader_retention_time"`
	StartOffset            int64         `envconfig:"reader_start_offset"`
	ReadBackoffMin         time.Duration `envconfig:"reader_read_backoff_min"`
	ReadBackoffMax         time.Duration `envconfig:"reader_read_backoff_max"`
	ErrorLogger            *log.Logger
	IsolationLevel         kafka.IsolationLevel
	MaxAttempts            int `envconfig:"max_attempts"`
}

type WriterConfig struct {
	Brokers           []string `envconfig:"broker_list" required:"true"`
	Topic             string
	Dialer            *kafka.Dialer
	Balancer          kafka.Balancer
	QueueCapacity     int           `envconfig:"writer_queue_capacity"`
	BatchSize         int           `envconfig:"writer_batch_size"`
	BatchBytes        int           `envconfig:"writer_batch_bytes"`
	BatchTimeout      time.Duration `envconfig:"writer_batch_timeout"`
	ReadTimeout       time.Duration `envconfig:"writer_read_timeout"`
	WriteTimeout      time.Duration `envconfig:"writer_writer_timeout"`
	RebalanceInterval time.Duration `envconfig:"writer_rebalance_interval"`
	RequiredAcks      int           `envconfig:"writer_required_acks"`
	Async             bool          `envconfig:"writer_async"`
	ErrorLogger       *log.Logger
}

type Split struct {
	InputTopic  string
	Extractor   Extractor `yaml:"extractor"`
	OutputTopic string    `yaml:"output_topic"`
	Action      string    `yaml:"action"`
}

type Spliter struct {
	Splits     []Split           `yaml:"splits"`
	InputTopic string            `yaml:"input_topic"`
	Actions    map[string]string `yaml:"actions"`
}

type SpliterCollection struct {
	Spliters []Spliter `yaml:"spliters_templates"`
}

type Extractor struct {
	Pattern  string `yaml:"pattern"`
	UseRegex bool   `yaml:"use_regex"`
}

type FlaggedMessage struct {
	KafkaMessage *kafka.Message
	Matched      bool
}

type ReaderWriterAssociation struct {
	ReaderConfig   kafka.ReaderConfig
	WriterChannels []chan *kafka.Message
}

func GetLogger() *log.Logger {
	logger := log.New(os.Stderr, "logger: ", log.Ldate|log.Ltime|log.Lmicroseconds|log.Llongfile)
	return logger
}

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
var memprofile = flag.String("memprofile", "", "write memory profile to this file")

func main() {
	flag.Parse()
	if *cpuprofile != "" || *memprofile != "" {
		fmem, err := os.Create(*memprofile)
		defer fmem.Close()

		if err != nil {
			log.Fatal(err)
		}

		if *cpuprofile != "" {
			f, err := os.Create(*cpuprofile)
			if err != nil {
				log.Fatal(err)
			}
			pprof.StartCPUProfile(f)
		}

		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGUSR1)

		go func() {
			s := <-c
			log.Print(s)
			if *cpuprofile != "" {
				pprof.StopCPUProfile()
			}

			if *memprofile != "" {
				pprof.WriteHeapProfile(fmem)
				fmem.Close()
			}
			os.Exit(0)
		}()
	}

	loggerBasic := GetLogger()
	loggerBasic.Println("Starting streamer...")

	groupPrefix := ""
	templateReaderConfig := ReaderConfig{}
	templateWriterConfig := WriterConfig{}
	certificates := make([]tls.Certificate, 1)
	dialer := &kafka.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	splitConf := os.Getenv("SPLIT_CONF")
	groupPrefix = os.Getenv("GROUP_PREFIX")
	sslSkipVerify := true
	useSSL := os.Getenv("SSL")
	sslPrivateKeyEncoded := os.Getenv("SSL_PRIVATE_KEY")
	sslClientCertEncoded := os.Getenv("SSL_CLIENT_CERT")
	sslTrustedCAEncoded := os.Getenv("SSL_TRUSTED_CA")
	debug := os.Getenv("DEBUG")

	if debug == "true" {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}

	defer logger.Sync()

	if splitConf == "" {
		logger.Fatal("SPLIT_CONF env var is missing!")
	}

	err := envconfig.Process("pannet-kafka-streamer", &templateReaderConfig)

	if err != nil {
		logger.Fatal(err.Error())
	}

	err = envconfig.Process("pannet-kafka-streamer", &templateWriterConfig)

	if err != nil {
		logger.Fatal(err.Error())
	}

	if groupPrefix != "" {
		if grpPrefixLength := len(groupPrefix); grpPrefixLength > 64 {
			logger.Fatal(
				"Maximal length of GROUP_PREFIX should be 64, now is",
				zap.Int("GROUP_PREFIX_LENGTH", grpPrefixLength))
		}
	}

	if useSSL == "true" {
		if sslPrivateKeyEncoded == "" {
			logger.Fatal("ENV var SSL_PRIVATE_KEY must be set!")
		}

		if sslClientCertEncoded == "" {
			logger.Fatal("ENV var SSL_CLIENT_CERT must be set!")
		}

		if sslTrustedCAEncoded == "" {
			logger.Fatal("ENV var SSL_TRUSTED_CA must be set!")
		}

		if os.Getenv("SSL_SKIP_VERIFY") != "" {
			if os.Getenv("SSL_SKIP_VERIFY") == "true" {
				sslSkipVerify = true
			}
		}

		sslPrivateKey, errPriv := base64.StdEncoding.DecodeString(sslPrivateKeyEncoded)

		if errPriv != nil {
			logger.Fatal(errPriv.Error())
		}

		sslClientCert, errClient := base64.StdEncoding.DecodeString(sslClientCertEncoded)

		if errClient != nil {
			logger.Fatal(errClient.Error())
		}

		sslTrustedCA, errTrust := base64.StdEncoding.DecodeString(sslTrustedCAEncoded)

		if errTrust != nil {
			logger.Fatal(errTrust.Error())
		}

		myCert, err := tls.X509KeyPair(sslClientCert, sslPrivateKey)

		rootCertPool := x509.NewCertPool()

		if err != nil {
			logger.Fatal(err.Error())
		}

		if ok := rootCertPool.AppendCertsFromPEM(sslTrustedCA); !ok {
			logger.Fatal("Failed to append root CA cert at trust.pem.")
		}

		certificates[0] = myCert

		dialer.DualStack = true
		dialer.TLS = &tls.Config{
			RootCAs:            rootCertPool,
			Certificates:       certificates,
			InsecureSkipVerify: sslSkipVerify,
		}
	}

	spliters := SpliterCollection{}

	data, err := base64.StdEncoding.DecodeString(splitConf)

	if err != nil {
		logger.Fatal(err.Error())
	}

	err = yaml.Unmarshal([]byte(data), &spliters)

	if err != nil {
		logger.Fatal(
			"Problem with split conf %s: %s",
			zap.String("SPLIT_CONF", splitConf),
			zap.String("Error: ", err.Error()),
		)
	}

	// logger.Debug(
	// 	"Spliters: ",
	// 	zap.Any("Spliters: ", spliters),
	// )

	done := make(chan bool)
	errChannel := make(chan error)

	for _, spliter := range spliters.Spliters {
		unmatchChannel := make(chan FlaggedMessage, 20)
		assoc := ReaderWriterAssociation{}

		readerConfig := templateReaderConfig
		readerConfig.Topic = spliter.InputTopic
		readerConfig.GroupID = fmt.Sprintf("%s-%s", groupPrefix, spliter.InputTopic)
		readerConfig.Dialer = dialer
		readerConfig.ErrorLogger = loggerBasic
		readerKafkaConfig := &kafka.ReaderConfig{}
		copier.Copy(readerKafkaConfig, &readerConfig)

		if debug == "true" {
			readerKafkaConfig.Logger = loggerBasic
		}

		assoc.ReaderConfig = *readerKafkaConfig
		assoc.WriterChannels = make([]chan *kafka.Message, 0)

		for _, split := range spliter.Splits {
			split.InputTopic = spliter.InputTopic
			writeChannel := make(chan *kafka.Message, 20)
			assoc.WriterChannels = append(assoc.WriterChannels, writeChannel)

			if split.OutputTopic == "" {
				if split.Action == "" {
					split.OutputTopic = spliter.Actions["matched"]
				} else {
					split.OutputTopic = spliter.Actions[split.Action]
				}
			}

			writerConfig := templateWriterConfig
			writerConfig.Topic = split.OutputTopic
			writerConfig.Dialer = dialer
			writerConfig.ErrorLogger = loggerBasic

			writerKafkaConfig := &kafka.WriterConfig{}

			copier.Copy(writerKafkaConfig, &writerConfig)

			if debug == "true" {
				writerKafkaConfig.Logger = loggerBasic
			}

			go produce(done, writeChannel, writerKafkaConfig, split, unmatchChannel, errChannel)
		}

		go consume(assoc, errChannel)

		if topicName, ok := spliter.Actions["unmatched"]; ok {
			writerConfig := templateWriterConfig
			writerConfig.Topic = topicName
			writerConfig.Dialer = dialer
			writerConfig.ErrorLogger = loggerBasic

			// logger.Debug(
			// 	"Output unmatch topic",
			// 	zap.String("Unmatch", topicName),
			// )

			writerKafkaConfig := &kafka.WriterConfig{}
			copier.Copy(writerKafkaConfig, &writerConfig)

			if debug == "true" {
				writerKafkaConfig.Logger = loggerBasic
			}

			go unmatched(unmatchChannel, writerKafkaConfig, spliter, errChannel)
		}
	}

	select {
	case <-done:
	case err := <-errChannel:
		if err != nil {
			logger.Fatal(err.Error())
		}
	}
}

func consume(assoc ReaderWriterAssociation, errChannel chan error) {
	reader := kafka.NewReader(assoc.ReaderConfig)
	defer reader.Close()

	for {
		m, err := reader.FetchMessage(context.Background())

		if err != nil {
			errChannel <- Error{fmt.Sprintf("Error fetching message: %s", err)}
		}

		for _, writeChannel := range assoc.WriterChannels {
			writeChannel <- &m
		}

		reader.CommitMessages(context.Background(), m)
	}
}

func produce(done chan bool, inputMsgChan chan *kafka.Message, writerConfig *kafka.WriterConfig, split Split, unmatched chan FlaggedMessage, errChannel chan error) {
	// logger.Debug(
	// 	"Starting producer thread for",
	// 	zap.String("pattern", split.Extractor.Pattern),
	// )

	w := kafka.NewWriter(*writerConfig)

	defer w.Close()

	pattern := split.Extractor.Pattern
	var regex *regexp.Regexp
	useRegex := split.Extractor.UseRegex

	if split.Extractor.UseRegex {
		reg, err := regexp.Compile(pattern)
		regex = reg
		if err != nil {
			errChannel <- Error{fmt.Sprintf("Failure compiling pattern %s", pattern)}
		}
	}

	for {
		m := <-inputMsgChan

		matched := false

		if useRegex {
			// logger.Debug(
			// 	"Using regex: ",
			// 	zap.String("regex", pattern),
			// )
			matched = regex.Match(m.Value)
		} else {
			// logger.Debug(
			// 	"Using substring: ",
			// 	zap.String("regex", pattern),
			// )
			matched = strings.Contains(string(m.Value), pattern)
		}

		newMsg := kafka.Message{
			Key:   m.Key,
			Value: m.Value,
		}

		flagged := FlaggedMessage{
			KafkaMessage: m,
			Matched:      matched,
		}

		if matched == true {
			// logger.Debug(
			// 	"Message matched",
			// 	zap.String("Matched", string(newMsg.Value)),
			// )
			// logger.Debug(
			// 	"Source topic:",
			// 	zap.String("Topic", split.InputTopic),
			// )
			// logger.Debug(
			// 	"Output topic:",
			// 	zap.String("Topic", split.OutputTopic),
			// )

			err := w.WriteMessages(context.Background(),
				newMsg,
			)

			if err != nil {
				errChannel <- Error{fmt.Sprintf("%s", err)}
			}
		}

		unmatched <- flagged
	}
}

func unmatched(unmatched chan FlaggedMessage, writerConfig *kafka.WriterConfig, spliter Spliter, errChannel chan error) {
	w := kafka.NewWriter(*writerConfig)
	defer w.Close()

	numSplits := len(spliter.Splits)
	candidatesAll := make(map[string][]bool)
	var buffer bytes.Buffer

	for {
		m := <-unmatched

		buffer.WriteString(strconv.Itoa(int(m.KafkaMessage.Offset)))
		buffer.WriteString("-")
		buffer.WriteString(strconv.Itoa(int(m.KafkaMessage.Partition)))
		msgName := buffer.String()
		buffer.Reset()

		// logger.Debug(
		// 	"Dumping all unmatched before clean:",
		// 	zap.Any("Unmatched", candidatesUnmatched),
		// )

		candidatesAll[msgName] = append(candidatesAll[msgName], m.Matched)

		// logger.Debug(
		// 	"Number of splits:",
		// 	zap.Any("Splits", len(spliter.Splits)),
		// )
		//
		// logger.Debug(
		// 	"Number of not matchet already:",
		// 	zap.Int("Splits", len(candidatesAll[msgName])),
		// )

		if numSplits == len(candidatesAll[msgName]) {
			nonMatched := 0

			for _, matching := range candidatesAll[msgName] {
				if matching == false {
					nonMatched++
				}
			}

			if numSplits == nonMatched {
				// logger.Debug("Writing unmatched")
				newMsg := kafka.Message{
					Key:   m.KafkaMessage.Key,
					Value: m.KafkaMessage.Value,
				}

				err := w.WriteMessages(context.Background(),
					newMsg,
				)

				if err != nil {
					errChannel <- Error{fmt.Sprintf("%s", err)}
				}
			}

			delete(candidatesAll, msgName)
		}

		// logger.Debug(
		// 	"Dumping all unmatched after clean:",
		// 	zap.Any("Unmatched", candidatesUnmatched),
		// )
	}
}
