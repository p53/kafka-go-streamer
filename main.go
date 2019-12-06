package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"crypto/tls"
	"crypto/x509"

	"github.com/jinzhu/copier"
	"github.com/kelseyhightower/envconfig"
	"github.com/segmentio/kafka-go"
	_ "github.com/segmentio/kafka-go/gzip"
	_ "github.com/segmentio/kafka-go/lz4"
	_ "github.com/segmentio/kafka-go/snappy"
	_ "github.com/segmentio/kafka-go/zstd"
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
	groupSuffix := ""
	templateReaderConfig := ReaderConfig{}

	certificates := make([]tls.Certificate, 1)
	dialer := &kafka.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 60 * time.Second,
	}

	splitConf := os.Getenv("SPLIT_CONF")
	groupPrefix = os.Getenv("GROUP_PREFIX")
	groupSuffix = os.Getenv("GROUP_SUFFIX")
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

	if groupPrefix != "" {
		if grpPrefixLength := len(groupPrefix); grpPrefixLength > 64 {
			logger.Fatal(
				"Maximal length of GROUP_PREFIX should be 64, now is",
				zap.Int("GROUP_PREFIX_LENGTH", grpPrefixLength))
		}
	}

	if groupSuffix != "" {
		if grpSuffixLength := len(groupSuffix); grpSuffixLength > 64 {
			logger.Fatal(
				"Maximal length of GROUP_SUFFIX should be 64, now is",
				zap.Int("GROUP_SUFFIX_LENGTH", grpSuffixLength))
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

	logger.Debug(
		"Spliters: ",
		zap.Any("Spliters: ", spliters),
	)

	done := make(chan bool)
	errChannel := make(chan error)

	for _, spliter := range spliters.Spliters {
		readerConfig := templateReaderConfig
		readerConfig.Topic = spliter.InputTopic
		readerConfig.GroupID = fmt.Sprintf("%s-streamer-%s", groupPrefix, groupSuffix)
		readerConfig.Dialer = dialer
		readerConfig.ErrorLogger = loggerBasic
		readerKafkaConfig := &kafka.ReaderConfig{}
		copier.Copy(readerKafkaConfig, &readerConfig)

		if debug == "true" {
			readerKafkaConfig.Logger = loggerBasic
		}

		writeChannel := make(chan *kafka.Message, 20)

		go produce(done, writeChannel, dialer, spliter, errChannel)
		go consume(readerKafkaConfig, writeChannel, errChannel)
	}

	select {
	case <-done:
	case err := <-errChannel:
		if err != nil {
			logger.Fatal(err.Error())
		}
	}
}

func consume(readerKafkaConfig *kafka.ReaderConfig, writeChannel chan *kafka.Message, errChannel chan error) {
	reader := kafka.NewReader(*readerKafkaConfig)
	defer reader.Close()

	for {
		m, err := reader.FetchMessage(context.Background())

		if err != nil {
			errChannel <- Error{fmt.Sprintf("Error fetching message: %s", err)}
		}

		writeChannel <- &m

		reader.CommitMessages(context.Background(), m)

		if err != nil {
			errChannel <- Error{fmt.Sprintf("Error commiting message: %s", err)}
		}
	}
}

func produce(done chan bool, inputMsgChan chan *kafka.Message, dialer *kafka.Dialer, spliter Spliter, errChannel chan error) {
	loggerBasic := GetLogger()
	writers := make([]*kafka.Writer, 0)
	regexes := make([]*regexp.Regexp, 0)
	batches := make([][]kafka.Message, 0)
	batchUnmatch := []kafka.Message{}
	batchTimers := make([]*time.Timer, 0)
	batchUnmatchTimer := time.NewTimer(0)
	var unmatchedWriter *kafka.Writer

	templateWriterConfig := WriterConfig{}
	err := envconfig.Process("pannet-kafka-streamer", &templateWriterConfig)

	if err != nil {
		logger.Fatal(err.Error())
	}

	for index, split := range spliter.Splits {
		split.InputTopic = spliter.InputTopic

		if split.OutputTopic == "" {
			if split.Action == "" {
				logger.Debug(
					"Empty action, setting from matched topic",
					zap.String("Action:", spliter.Actions["matched"]),
				)
				spliter.Splits[index].OutputTopic = spliter.Actions["matched"]
			} else {
				// if split refers to action in actions field of spliter
				// use that topic, if split refers to action which is not in actions
				// field of spliter, append just nil, later we will look if there is writer
				// or nil and if nil, we skip writing (this is heritage from old streamer...)
				if val, ok := spliter.Actions[split.Action]; ok {
					logger.Debug(
						"Setting output from action",
						zap.String("Action:", split.Action),
						zap.String("out topic:", val),
					)
					spliter.Splits[index].OutputTopic = val
				} else {
					logger.Warn(
						"There is no action, output topic will be empty",
						zap.String("Missing action in spliter", split.Action),
					)
					spliter.Splits[index].OutputTopic = ""
					writers = append(writers, nil)
					batches = append(batches, nil)
					batchTimers = append(batchTimers, nil)
				}
			}
		}

		if split.OutputTopic != "" {
			writerConfig := templateWriterConfig
			writerConfig.Topic = split.OutputTopic
			writerConfig.Dialer = dialer
			writerConfig.ErrorLogger = loggerBasic

			writerKafkaConfig := &kafka.WriterConfig{}

			copier.Copy(writerKafkaConfig, &writerConfig)

			if debug := os.Getenv("DEBUG"); debug == "true" {
				writerKafkaConfig.Logger = loggerBasic
			}

			w := kafka.NewWriter(*writerKafkaConfig)
			batch := []kafka.Message{}
			batchTimer := time.NewTimer(0)
			<-batchTimer.C
			batchTimer.Reset(10 * time.Second)
			defer batchTimer.Stop()
			defer w.Close()
			writers = append(writers, w)
			batches = append(batches, batch)
			batchTimers = append(batchTimers, batchTimer)
		}

		var reg *regexp.Regexp

		if split.Extractor.UseRegex {
			pattern := split.Extractor.Pattern
			reg, err = regexp.Compile(pattern)
			if err != nil {
				errChannel <- Error{fmt.Sprintf("Failure compiling pattern %s", pattern)}
			}
		}

		regexes = append(regexes, reg)
	}

	if topicName, ok := spliter.Actions["unmatched"]; ok {
		writerConfig := templateWriterConfig
		writerConfig.Topic = topicName
		writerConfig.Dialer = dialer
		writerConfig.ErrorLogger = loggerBasic

		logger.Debug(
			"Output unmatch topic",
			zap.String("Unmatch", topicName),
		)

		unmatchedWriterConfig := &kafka.WriterConfig{}
		copier.Copy(unmatchedWriterConfig, &writerConfig)

		if debug := os.Getenv("DEBUG"); debug == "true" {
			unmatchedWriterConfig.Logger = loggerBasic
		}
		unmatchedWriter = kafka.NewWriter(*unmatchedWriterConfig)

		<-batchUnmatchTimer.C
		batchUnmatchTimer.Reset(10 * time.Second)
		defer batchUnmatchTimer.Stop()
	}

	batchSize := templateWriterConfig.BatchSize

	if batchSize == 0 {
		batchSize = 100
	}

	for {
		m := <-inputMsgChan

		newMsg := kafka.Message{
			Key:   m.Key,
			Value: m.Value,
		}

		matched := false
		numUnmatched := 0

		for index, split := range spliter.Splits {

			if split.Extractor.UseRegex {
				logger.Debug(
					"Using regex: ",
					zap.String("regex", split.Extractor.Pattern),
				)
				matched = regexes[index].Match(m.Value)
			} else {
				logger.Debug(
					"Using substring: ",
					zap.String("substring", split.Extractor.Pattern),
				)
				matched = strings.Contains(string(m.Value), split.Extractor.Pattern)
			}

			if matched == true {
				logger.Debug(
					"Message matched",
					zap.String("Matched", string(newMsg.Value)),
				)
				logger.Debug(
					"Source topic:",
					zap.String("Topic", spliter.InputTopic),
				)
				logger.Debug(
					"Output topic:",
					zap.String("Topic", split.OutputTopic),
				)
				if writers[index] != nil {
					if len(batches[index]) < batchSize {
						batches[index] = append(batches[index], newMsg)
					}

					mustFlush := false
					batchTimerRunning := true

					select {
					case <-batchTimers[index].C:
						mustFlush = true
						batchTimerRunning = false
						logger.Debug(
							"Running timer",
						)
					default:
						logger.Debug(
							"Default select",
						)
						if len(batches[index]) == batchSize {
							mustFlush = true
							logger.Debug(
								"Running batch",
								zap.Int("Size of batch", batchSize),
							)
						}
					}

					if mustFlush {
						err := writers[index].WriteMessages(context.Background(), batches[index]...)
						logger.Debug(
							"Flushing",
							zap.Int("Size of Flushed batch", len(batches[index])),
							zap.String("Flushed input topic", spliter.InputTopic),
						)
						if err != nil {
							errChannel <- Error{fmt.Sprintf("%s", err)}
						}
						batches[index] = []kafka.Message{}

						if !batchTimerRunning {
							batchTimers[index].Reset(10 * time.Second)
						} else {
							if stopped := batchTimers[index].Stop(); !stopped {
								<-batchTimers[index].C
							}
							batchTimers[index].Reset(10 * time.Second)
						}
					}
				}

				break
			}

			numUnmatched++

			if numUnmatched == len(spliter.Splits) && unmatchedWriter != nil {
				if len(batchUnmatch) < batchSize {
					batchUnmatch = append(batchUnmatch, newMsg)
				}

				mustFlush := false
				batchTimerRunning := true

				select {
				case <-batchUnmatchTimer.C:
					mustFlush = true
					batchTimerRunning = false
					logger.Debug(
						"Running timer unmatch",
					)
				default:
					logger.Debug(
						"Default select unmatch",
					)
					if len(batchUnmatch) == batchSize {
						mustFlush = true
						logger.Debug(
							"Running batch unmatch",
							zap.Int("Size of batch unmatch", batchSize),
						)
					}
				}

				if mustFlush {
					err := unmatchedWriter.WriteMessages(context.Background(), batchUnmatch...)
					logger.Debug(
						"Flushing",
						zap.Int("Size of Flushed unmatch batch", len(batchUnmatch)),
						zap.String("Flushed unmatch input topic", spliter.InputTopic),
					)
					if err != nil {
						errChannel <- Error{fmt.Sprintf("%s", err)}
					}
					batchUnmatch = []kafka.Message{}

					if !batchTimerRunning {
						batchUnmatchTimer.Reset(10 * time.Second)
					} else {
						if stopped := batchUnmatchTimer.Stop(); !stopped {
							<-batchUnmatchTimer.C
						}
						batchUnmatchTimer.Reset(10 * time.Second)
					}
				}
			}
		}
	}
}
