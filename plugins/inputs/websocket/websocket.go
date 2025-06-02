package websocket

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	ws "github.com/gorilla/websocket"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/common/proxy"
	"github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type WebSocketInput struct {
	URL             string                    `toml:"url"`
	Headers         map[string]*config.Secret `toml:"headers"`
	TriggerBody     string                    `toml:"trigger_body"`
	UseTextFrames   bool                      `toml:"use_text_frames"`
	ReadTimeout     config.Duration           `toml:"read_timeout"`
	TriggerInterval config.Duration           `toml:"trigger_interval"`
	InstanceDelay   config.Duration           `toml:"instance_delay"`
	Log             telegraf.Logger           `toml:"-"`
	tls.ClientConfig
	proxy.HTTPProxy
	proxy.Socks5ProxyConfig

	parser    telegraf.Parser
	conn      *ws.Conn
	writeMu   sync.Mutex
	acc       telegraf.Accumulator
	stop      chan struct{}
	messageId int
	mu        sync.Mutex // Mutex to ensure safe increment of messageId
	connected bool
	connMu    sync.RWMutex
}

func (w *WebSocketInput) SampleConfig() string {
	return ` 
  url = "wss://example.com/ws"
  trigger_body = "{\"type\":\"subscribe\"}"
  read_timeout = "30s"
  trigger_interval = "1s"         # New setting
  use_text_frames = true
  
  [inputs.websocket.headers]
    Authorization = "Bearer YOUR_TOKEN"

  # Optional TLS config
  # [inputs.websocket.tls]
  #   ...
`
}

func (w *WebSocketInput) Description() string {
	return "Receive metrics from a WebSocket endpoint"
}

func (w *WebSocketInput) SetParser(p telegraf.Parser) {
	w.parser = p
}

var (
	instanceCounter int
	counterMu       sync.Mutex
)

func (w *WebSocketInput) isConnected() bool {
	w.connMu.RLock()
	defer w.connMu.RUnlock()
	return w.connected
}

func (w *WebSocketInput) setConnected(state bool) {
	w.connMu.Lock()
	w.connected = state
	w.connMu.Unlock()
}

func (w *WebSocketInput) triggerLoop(initialDelay time.Duration) {
	// Wait for initial stagger delay
	interval := time.Duration(w.TriggerInterval)

	// Align to next clean interval
	now := time.Now()
	nextTrigger := now.Truncate(interval).Add(interval).Add(initialDelay)
	time.Sleep(time.Until(nextTrigger))

	for {
		if err := w.sendTrigger(); err != nil {
			w.Log.Errorf("Trigger error at %s: %v", time.Now().Format("15:04:05.000000"), err)
		}

		// Calculate next trigger time
		nextTrigger = nextTrigger.Add(interval)
		sleepFor := time.Until(nextTrigger)

		select {
		case <-w.stop:
			return
		case <-time.After(sleepFor):
			// Continue to next iteration
		}
	}
}

func (w *WebSocketInput) Start(acc telegraf.Accumulator) error {
	w.acc = acc
	w.stop = make(chan struct{})

	counterMu.Lock()
	instanceCounter++
	instanceNum := instanceCounter
	counterMu.Unlock()

	delay := w.InstanceDelay
	if delay == 0 {
		delay = config.Duration(0 * time.Millisecond)
	}
	initialDelay := time.Duration(instanceNum-1) * time.Duration(delay)
	w.Log.Infof("Delaying WebSocket connection #%d by %v", instanceNum, initialDelay)

	go func() {
		time.Sleep(initialDelay)
		w.receiver() // starts connection
	}()

	go w.triggerLoop(initialDelay)

	return nil
}

func (w *WebSocketInput) Stop() {
	close(w.stop)
	w.writeMu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
	w.writeMu.Unlock()
}

func (w *WebSocketInput) Gather(_ telegraf.Accumulator) error {
	// Gather is not used in WebSocket input
	// it was creating problem  in triggering at correct intervals
	return nil
}

func (w *WebSocketInput) receiver() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-w.stop:
			w.Log.Infof("WebSocket receiver stopped")
			return
		default:
			if err := w.connect(); err != nil {
				w.Log.Errorf("Connect error: %v", err)
				time.Sleep(backoff)
				// Exponential backoff
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			backoff = time.Second
			w.setConnected(true)
			w.readLoop()
			w.setConnected(false)
		}
	}
}

func (w *WebSocketInput) connect() error {
	tlsCfg, err := w.ClientConfig.TLSConfig()
	if err != nil {
		return err
	}

	dialProxy, err := w.HTTPProxy.Proxy()
	if err != nil {
		return err
	}

	dialer := &ws.Dialer{
		Proxy:            dialProxy,
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  tlsCfg,
	}

	if w.Socks5ProxyEnabled {
		netDialer, err := w.Socks5ProxyConfig.GetDialer()
		if err != nil {
			return fmt.Errorf("SOCKS5 dialer error: %w", err)
		}
		dialer.NetDial = netDialer.Dial
	}

	headers := http.Header{}
	if w.TriggerBody != "" {
		headers.Add("Origin", "Test") // Only if needed
	}

	// Keep only custom headers, avoid standard WS ones
	for k, v := range w.Headers {
		val, err := v.Get()
		if err != nil {
			return fmt.Errorf("header secret error: %w", err)
		}
		headers.Set(k, val.String())
		val.Destroy()
	}

	conn, resp, err := dialer.Dial(w.URL, headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("dial error: %w\nResponse: %s", err, body)
		}
		return fmt.Errorf("dial error: %w", err)
	}
	w.conn = conn

	if w.TriggerBody != "" {
		msgType := ws.BinaryMessage
		if w.UseTextFrames {
			msgType = ws.TextMessage
		}

		w.Log.Infof("Sending trigger body: %s", w.TriggerBody)
		w.writeMu.Lock()
		if err := w.conn.WriteMessage(msgType, []byte(w.TriggerBody)); err != nil {
			return fmt.Errorf("failed to send trigger: %w", err)
		}
		w.writeMu.Unlock()
		w.Log.Infof("Trigger body sent")
	}

	return nil
}

func (w *WebSocketInput) readLoop() {
	defer func() {
		if w.conn != nil {
			_ = w.conn.Close()
			w.conn = nil
			w.Log.Infof("WebSocket connection closed")
		}
	}()

	for {
		select {
		case <-w.stop:
			w.Log.Infof("Stopping read loop")
			return
		default:
			if w.ReadTimeout > 0 {
				_ = w.conn.SetReadDeadline(time.Now().Add(time.Duration(w.ReadTimeout)))
			}
			_, msg, err := w.conn.ReadMessage()
			if err != nil {
				w.Log.Errorf("WebSocket read error: %v", err)
				return
			}

			w.Log.Debugf("Received message at %s with msg: %d", time.Now().Format("2006-01-02 15:04:05.000"), len(msg))
			metrics, err := w.parser.Parse(msg)
			if err != nil {
				w.Log.Errorf("Parse error: %v", err)
				continue
			}

			for _, m := range metrics {
				if len(m.Fields()) > 0 {
					m.AddField("arrival_date", time.Now().Format("2006-01-02 15:04:05.000"))
				} else {
					w.Log.Debugf("Dropped metric with no fields: name=%s, tags=%v", m.Name(), m.Tags())
				}
				w.acc.AddMetric(m)
			}

		}
	}

}

// Send the trigger with the incremented messageId
func (w *WebSocketInput) sendTrigger() error {
	if !w.isConnected() {
		return fmt.Errorf("not connected")
	}

	w.mu.Lock()
	w.messageId++
	messageId := w.messageId
	w.mu.Unlock()

	triggerData := make(map[string]interface{})
	if err := json.Unmarshal([]byte(w.TriggerBody), &triggerData); err != nil {
		return fmt.Errorf("failed to parse trigger body: %w", err)
	}

	triggerData["message_id"] = messageId
	updatedBody, err := json.Marshal(triggerData)
	if err != nil {
		return fmt.Errorf("failed to marshal trigger: %w", err)
	}

	msgType := ws.TextMessage
	if !w.UseTextFrames {
		msgType = ws.BinaryMessage
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	w.Log.Debugf("Sending trigger %s at %v", w.TriggerBody, time.Now().Format("2006-01-02 15:04:05.000"))
	if err := w.conn.WriteMessage(msgType, updatedBody); err != nil {
		w.setConnected(false)
		return fmt.Errorf("write failed: %w", err)
	}

	return nil
}
func init() {
	inputs.Add("websocket", func() telegraf.Input {
		return &WebSocketInput{
			ReadTimeout: config.Duration(30 * time.Second),
		}
	})
}
