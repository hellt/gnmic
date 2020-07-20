package nats_output

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/karimra/gnmic/collector"
	"github.com/karimra/gnmic/outputs"
	"github.com/mitchellh/mapstructure"
	"github.com/nats-io/nats.go"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	natsConnectTimeout = 5 * time.Second
	natsConnectWait    = 2 * time.Second

	natsReconnectBufferSize = 100 * 1024 * 1024

	defaultSubjectName = "gnmic-telemetry"
)

type msg struct {
	Tags     outputs.Meta  `json:"tags,omitempty"`
	Msg      proto.Message `json:"msg,omitempty"`
	MsgBytes []byte        `json:"msg_bytes,omitempty"`
}

func init() {
	outputs.Register("nats", func() outputs.Output {
		return &NatsOutput{
			Cfg: &Config{},
		}
	})
}

// NatsOutput //
type NatsOutput struct {
	Cfg      *Config
	ctx      context.Context
	cancelFn context.CancelFunc
	conn     *nats.Conn
	metrics  []prometheus.Collector
	logger   *log.Logger
}

// Config //
type Config struct {
	Name            string        `mapstructure:"name,omitempty"`
	Address         string        `mapstructure:"address,omitempty"`
	SubjectPrefix   string        `mapstructure:"subject-prefix,omitempty"`
	Subject         string        `mapstructure:"subject,omitempty"`
	Username        string        `mapstructure:"username,omitempty"`
	Password        string        `mapstructure:"password,omitempty"`
	ConnectTimeWait time.Duration `mapstructure:"connect-time-wait,omitempty"`
	Format          string        `mapstructure:"format,omitempty"`
}

func (n *NatsOutput) String() string {
	b, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return string(b)
}

// Init //
func (n *NatsOutput) Init(cfg map[string]interface{}, logger *log.Logger) error {
	err := mapstructure.Decode(cfg, n.Cfg)
	if err != nil {
		return err
	}
	if n.Cfg.ConnectTimeWait == 0 {
		n.Cfg.ConnectTimeWait = natsConnectWait
	}
	if n.Cfg.Subject == "" && n.Cfg.SubjectPrefix == "" {
		n.Cfg.Subject = defaultSubjectName
	}
	n.logger = log.New(os.Stderr, "nats_output ", log.LstdFlags|log.Lmicroseconds)
	if logger != nil {
		n.logger.SetOutput(logger.Writer())
		n.logger.SetFlags(logger.Flags())
	}
	if n.Cfg.Format == "" {
		n.Cfg.Format = "event"
	}
	if !(n.Cfg.Format == "event" || n.Cfg.Format == "json" || n.Cfg.Format == "proto") {
		return fmt.Errorf("unsupported output format: %s", n.Cfg.Format)
	}
	if n.Cfg.Name == "" {
		n.Cfg.Name = "gnmic-" + uuid.New().String()
	}
	n.ctx, n.cancelFn = context.WithCancel(context.Background())
	n.conn, err = n.createNATSConn(n.Cfg)
	if err != nil {
		return err
	}
	n.logger.Printf("initialized nats producer: %s", n.String())
	return nil
}

// Write //
func (n *NatsOutput) Write(rsp proto.Message, meta outputs.Meta) {
	if rsp == nil {
		return
	}
	if format, ok := meta["format"]; ok {
		if format == "textproto" {
			return
		}
	}
	ssb := strings.Builder{}
	ssb.WriteString(n.Cfg.SubjectPrefix)
	if n.Cfg.SubjectPrefix != "" {
		if s, ok := meta["source"]; ok {
			source := strings.ReplaceAll(fmt.Sprintf("%s", s), ".", "-")
			source = strings.ReplaceAll(source, " ", "_")
			ssb.WriteString(".")
			ssb.WriteString(source)
		}
		if subname, ok := meta["subscription-name"]; ok {
			ssb.WriteString(".")
			ssb.WriteString(fmt.Sprintf("%s", subname))
		}
	} else if n.Cfg.Subject != "" {
		ssb.WriteString(n.Cfg.Subject)
	}
	subject := strings.ReplaceAll(ssb.String(), " ", "_")
	b := make([]byte, 0)
	var err error
	switch n.Cfg.Format {
	case "proto":
		b, err = proto.Marshal(rsp)
	case "json":
		b, err = protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(rsp)
	case "event":
		switch sub := rsp.ProtoReflect().Interface().(type) {
		case *gnmi.SubscribeResponse:
			var subscriptionName string
			var ok bool
			if subscriptionName, ok = meta["subscription-name"]; !ok {
				subscriptionName = "default"
			}
			switch sub.Response.(type) {
			case *gnmi.SubscribeResponse_Update:
				events, err := collector.ResponseToEventMsgs(subscriptionName, sub, meta)
				if err != nil {
					n.logger.Printf("failed converting response to events: %v", err)
					return
				}
				b, err = json.MarshalIndent(events, "", "  ")
				if err != nil {
					n.logger.Printf("failed marshaling events: %v", err)
					return
				}
			case *gnmi.SubscribeResponse_SyncResponse:
				n.logger.Printf("received subscribe syncResponse with %v", meta)
			case *gnmi.SubscribeResponse_Error:
				gnmiErr := sub.GetError()
				n.logger.Printf("received subscribe response error with %v, code=%d, message=%v, data=%v ",
					meta, gnmiErr.Code, gnmiErr.Message, gnmiErr.Data)
			}
		}
	}
	if err != nil {
		n.logger.Printf("failed marshaling event: %v", err)
		return
	}
	err = n.conn.Publish(subject, b)
	if err != nil {
		log.Printf("failed to write to nats subject '%s': %v", subject, err)
		return
	}
	// n.logger.Printf("wrote %d bytes to nats_subject=%s", len(b), n.Cfg.Subject)
}

// Close //
func (n *NatsOutput) Close() error {
	n.cancelFn()
	n.conn.Close()
	return nil
}

// Metrics //
func (n *NatsOutput) Metrics() []prometheus.Collector { return n.metrics }

func (n *NatsOutput) createNATSConn(c *Config) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name(c.Name),
		nats.SetCustomDialer(n),
		nats.ReconnectWait(n.Cfg.ConnectTimeWait),
		nats.ReconnectBufSize(natsReconnectBufferSize),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			n.logger.Printf("NATS error: %v", err)
		}),
		nats.DisconnectHandler(func(c *nats.Conn) {
			n.logger.Println("Disconnected from NATS")
		}),
		nats.ClosedHandler(func(c *nats.Conn) {
			n.logger.Println("NATS connection is closed")
		}),
	}
	if c.Username != "" && c.Password != "" {
		opts = append(opts, nats.UserInfo(c.Username, c.Password))
	}
	nc, err := nats.Connect(c.Address, opts...)
	if err != nil {
		return nil, err
	}
	return nc, nil
}

// Dial //
func (n *NatsOutput) Dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(n.ctx)
	defer cancel()

	for {
		n.logger.Println("attempting to connect to", address)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		select {
		case <-n.ctx.Done():
			return nil, n.ctx.Err()
		default:
			d := &net.Dialer{}
			if conn, err := d.DialContext(ctx, network, address); err == nil {
				n.logger.Printf("successfully connected to NATS server %s", address)
				return conn, nil
			}
			time.Sleep(n.Cfg.ConnectTimeWait)
		}
	}
}

func (n *NatsOutput) marshal(rsp *gnmi.SubscribeResponse, meta outputs.Meta) ([]byte, error) {

	return nil, nil
}
