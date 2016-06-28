package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rcrowley/go-metrics"
	"github.com/uber-go/zap"
)

var (
	poolDuration     = 1 * time.Second
	log              zap.Logger
	maxMsgPerUpdates = 100

	// stats
	updateCount      = metrics.NewRegisteredCounter("telegram.updates.count", metrics.DefaultRegistry)
	msgPerUpdateRate = metrics.NewRegisteredGauge("telegram.messagePerUpdate.rate", metrics.DefaultRegistry)
)

func init() {
	log = zap.NewJSON(zap.AddCaller(), zap.AddStacks(zap.FatalLevel))
}

func SetLogger(l zap.Logger) {
	log = l.With(zap.String("module", "bot"))
}

/**
 * Telegram API specific data structure
 */

// TResponse represents response from telegram
type TResponse struct {
	Ok          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int64           `json:"error_code,omitempty"`
	Description string          `json:"description"`
}

// TUpdate represents an update event from telegram
type TUpdate struct {
	UpdateID int64    `json:"update_id"`
	Message  TMessage `json:"message"`
}

// TMessage is Telegram incomming message
type TMessage struct {
	MessageID       int64  `json:"message_id"`
	From            TUser  `json:"from"`
	Date            int64  `json:"date"`
	Chat            TChat  `json:"chat"`
	Text            string `json:"text"`
	ParseMode       string `json:"parse_mode,omitempty"`
	MigrateToChatID *int64 `json:"migrate_to_chat_id,omitempty"`
}

// TOutMessage is Telegram outgoing message
type TOutMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// TUser is Telegram User
type TUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// TChat represents Telegram chat session
type TChat struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	TUser
}

// TChatTypeMap maps betwwen string to bot.ChatType
var TChatTypeMap = map[string]ChatType{
	"private":    Private,
	"group":      Group,
	"supergroup": SuperGroup,
	"channel":    Channel,
}

// Telegram API
type Telegram struct {
	url        string
	input      map[Plugin]chan interface{}
	output     chan Message
	quit       chan struct{}
	lastUpdate int64
}

// NewTelegram creates telegram API Client
func NewTelegram(key string) *Telegram {
	if key == "" {
		log.Fatal("telegram API key must not be empty")
	}
	return &Telegram{
		url:    fmt.Sprintf("https://api.telegram.org/bot%s", key),
		input:  make(map[Plugin]chan interface{}),
		output: make(chan Message),
		quit:   make(chan struct{}),
	}
}

//AddPlugin add processing module to telegram
func (t *Telegram) AddPlugin(p Plugin) error {
	input, err := p.Init(t.output)
	if err != nil {
		return err
	}
	t.input[p] = input

	return nil
}

// Start consuming from telegram
func (t *Telegram) Start() {
	go t.poolOutbox()
	t.poolInbox()
}

func (t *Telegram) poolOutbox() {
	for {
		select {
		case m := <-t.output:
			outMsg := TOutMessage{
				ChatID:    m.Chat.ID,
				Text:      m.Text,
				ParseMode: string(m.Format),
			}

			var b bytes.Buffer
			if err := json.NewEncoder(&b).Encode(outMsg); err != nil {
				log.Error("encoding message", zap.Error(err))
				continue
			}
			log.Debug("sendMessage", zap.String("msg", b.String()))
			resp, err := http.Post(fmt.Sprintf("%s/sendMessage", t.url), "application/json; charset=utf-10", &b)
			if err != nil {
				log.Error("sendMessage failed", zap.String("ChatID", outMsg.ChatID), zap.Error(err))
				continue
			}
			metrics.GetOrRegisterCounter(fmt.Sprintf("telegram.sendMessage.http.%d", resp.StatusCode), metrics.DefaultRegistry).Inc(1)
			if err := t.parseOutbox(resp, outMsg.ChatID); err != nil {
				log.Error("parsing sendMessage response failed", zap.String("ChatID", outMsg.ChatID), zap.Error(err), zap.Object("msg", outMsg))
			}
		case <-t.quit:
			return
		}
	}
}

func (t *Telegram) poolInbox() {
	for {
		select {
		case <-t.quit:
			return
		default:
			resp, err := http.Get(fmt.Sprintf("%s/getUpdates?offset=%d", t.url, t.lastUpdate+1))
			if err != nil {
				log.Error("getUpdates failed", zap.Error(err))
				continue
			}
			metrics.GetOrRegisterCounter(fmt.Sprintf("telegram.getUpdates.http.%d", resp.StatusCode), metrics.DefaultRegistry).Inc(1)
			updateCount.Inc(1)
			nMsg, err := t.parseInbox(resp)
			if err != nil {
				log.Error("parsing updates response failed", zap.Error(err))
			}
			msgPerUpdateRate.Update(int64(nMsg))
			if nMsg != maxMsgPerUpdates {
				time.Sleep(poolDuration)
			}
		}
	}
}

func (t *Telegram) parseInbox(resp *http.Response) (int, error) {
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var tresp TResponse
	if err := decoder.Decode(&tresp); err != nil {
		return 0, err
	}

	if !tresp.Ok {
		log.Error("parsing response failed", zap.Int64("errorCode", tresp.ErrorCode), zap.String("description", tresp.Description))
		return 0, nil
	}

	var results []TUpdate
	json.Unmarshal(tresp.Result, &results)
	for _, update := range results {
		m := update.Message
		t.lastUpdate = update.UpdateID

		var msg interface{}
		message := Message{
			ID: strconv.FormatInt(m.MessageID, 10),
			From: User{
				ID:        strconv.FormatInt(m.From.ID, 10),
				FirstName: m.From.FirstName,
				LastName:  m.From.LastName,
				Username:  m.From.Username,
			},
			Date: time.Unix(m.Date, 0),
			Chat: Chat{
				ID:       strconv.FormatInt(m.Chat.ID, 10),
				Type:     TChatTypeMap[m.Chat.Type],
				Title:    m.Chat.Title,
				Username: m.Chat.Username,
			},
			Text: m.Text,
		}
		if m.MigrateToChatID != nil {
			newChanID := strconv.FormatInt(*(m.MigrateToChatID), 10)
			chanMigratedMsg := ChannelMigratedMessage{
				Message: message,
				FromID:  message.Chat.ID,
				ToID:    newChanID,
			}
			msg = &chanMigratedMsg
		}
		msg = &message
		log.Debug("update", zap.Object("msg", msg))
		for plugin, ch := range t.input {
			select {
			case ch <- msg:
			default:
				log.Warn("input channel full, skipping message", zap.String("plugin", plugin.Name()), zap.String("msgID", message.ID))
			}
		}
	}

	return len(results), nil
}

func (t *Telegram) parseOutbox(resp *http.Response, chatID string) error {
	defer resp.Body.Close()

	var tresp TResponse
	if err := json.NewDecoder(resp.Body).Decode(&tresp); err != nil {
		return fmt.Errorf("decoding response failed id:%s, %s", chatID, err)
	}
	if !tresp.Ok {
		return fmt.Errorf("code:%d description:%s", tresp.ErrorCode, tresp.Description)
	}

	return nil
}
