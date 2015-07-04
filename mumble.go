package mumble

import (
	"crypto/tls"
	"errors"
	"html"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/danryan/env"
	"github.com/layeh/gumble/gumble"
	"github.com/layeh/gumble/gumble_ffmpeg"
	"github.com/layeh/gumble/gumbleutil"
	"github.com/nightexcessive/hal"

	"github.com/nightexcessive/hal-mumble/htmlutil"
)

func init() {
	hal.RegisterAdapter("mumble", New)
}

// adapter struct
type adapter struct {
	hal.BasicAdapter
	server    string
	port      int
	password  string
	channel   string
	speakChan chan ttsRequest
	client    *gumble.Client
}

type ttsRequest struct {
	Text string

	Response chan error
}

type config struct {
	Password string `env:"key=HAL_MUMBLE_PASSWORD"`
	Server   string `env:"required key=HAL_MUMBLE_SERVER"`
	Port     int    `env:"key=HAL_MUMBLE_PORT default=64738"`
	Channel  string `env:"required key=HAL_MUMBLE_CHANNEL"`
}

// New returns an initialized adapter
func New(robot *hal.Robot) (hal.Adapter, error) {
	c := &config{}
	env.MustProcess(c)

	a := &adapter{
		server:    c.Server,
		port:      c.Port,
		password:  c.Password,
		channel:   strings.ToLower(c.Channel),
		speakChan: make(chan ttsRequest),
	}

	a.SetRobot(robot)
	return a, nil
}

var (
	errChannelNotFound = errors.New("mumble: Send: channel not found")
	errNotImplemented  = errors.New("mumble: not implemented")
)

// Send sends a regular response
func (a *adapter) Send(res *hal.Response, strings ...string) error {
	channel := a.client.Channels.Find(res.Message.Room)
	if channel == nil {
		return errChannelNotFound
	}

	for _, str := range strings {
		channel.Send(str, false)
		hal.Logger.Debug("mumble: sent message to channel")
	}

	return nil
}

// Reply sends a direct response
func (a *adapter) Reply(res *hal.Response, strings ...string) error {
	newStrings := make([]string, len(strings))
	for _, str := range strings {
		newStrings = append(newStrings, res.UserID()+`: `+str)
	}

	return a.Send(res, newStrings...)
}

// Emote is not implemented.
func (a *adapter) Emote(res *hal.Response, strings ...string) error {
	return errNotImplemented
}

// Topic sets the topic
func (a *adapter) Topic(res *hal.Response, strings ...string) error {
	return errNotImplemented
}

// Play plays phrases via text-to-speech
func (a *adapter) Play(res *hal.Response, strings ...string) error {
	if res.Message.Room != a.client.Self.Channel.Name {
		return errChannelNotFound
	}

	for _, str := range strings {
		if err := a.speak(str); err != nil {
			return err
		}
	}

	return nil
}

func (a *adapter) speak(str string) error {
	c := make(chan error, 1)
	a.speakChan <- ttsRequest{
		Text: str,

		Response: c,
	}

	hal.Logger.Debugf("mumble: speaking %q", str)

	return <-c
}

// Receive forwards a message to the robot
func (a *adapter) Receive(msg *hal.Message) error {
	return a.Robot.Receive(msg)
}

// Run starts the adapter
func (a *adapter) Run() error {
	// set up a connection to the Mumble gateway
	hal.Logger.Debug("mumble: starting connection")

	return a.startMumbleConnection()
}

// Stop shuts down the adapter
func (a *adapter) Stop() error {
	hal.Logger.Debug("mumble: stopping connection")

	return a.stopMumbleConnection()
}

func (a *adapter) startMumbleConnection() error {
	conn := gumble.NewClient(&gumble.Config{
		Address:  net.JoinHostPort(a.server, strconv.Itoa(a.port)),
		Username: a.Robot.Name,
		Password: a.password,

		TLSConfig: tls.Config{
			InsecureSkipVerify: true,
		},

		AudioInterval: 10 * time.Millisecond,
	})

	conn.Attach(gumbleutil.AutoBitrate)
	conn.Attach(&gumbleutil.Listener{
		Connect: func(e *gumble.ConnectEvent) {
			hal.Logger.Debug("mumble: connected")

			for _, channel := range e.Client.Channels {
				if strings.ToLower(channel.Name) == a.channel {
					e.Client.Self.Move(channel)
					break
				}
			}

			a.setSelfAvatar()

			go a.ttsGoRoutine()
		},

		Disconnect: func(e *gumble.DisconnectEvent) {
			close(a.speakChan)
		},

		TextMessage: func(e *gumble.TextMessageEvent) {
			message := a.newTextMessage(e)
			if message == nil {
				return
			}

			if err := a.Receive(message); err != nil {
				hal.Logger.Errorf("mumble: error in message receive: %v", err)
			}
		},

		UserChange: func(e *gumble.UserChangeEvent) {
			// TODO: Detect leaving
			if e.Type&gumble.UserChangeChannel != gumble.UserChangeChannel { // We only care about joining and leaving
				return
			}

			if e.User.Session == e.Client.Self.Session {
				return // Ignore our own events
			}

			if e.User.Channel.ID != e.Client.Self.Channel.ID {
				return // If they aren't joining this channel, ignore them
			}

			msg := &hal.Message{
				Type: hal.ENTER,

				User: hal.User{
					ID:   e.User.Name,
					Name: e.User.Name,
				},

				Room: e.User.Channel.Name,
			}

			if err := a.Receive(msg); err != nil {
				hal.Logger.Errorf("mumble: error in enter receive: %v", err)
			}
		},
	})

	err := conn.Connect()
	if err != nil {
		return err
	}

	a.client = conn
	hal.Logger.Debug("mumble: connecting...")

	return nil
}

func (a *adapter) stopMumbleConnection() error {
	hal.Logger.Debug("mumble: stopping Mumble connection")
	return a.client.Close()
}

func (a *adapter) newTextMessage(e *gumble.TextMessageEvent) *hal.Message {
	if e.Sender == nil {
		return nil
	}

	return &hal.Message{
		Type: hal.HEAR,

		User: hal.User{
			ID:   e.Sender.Name,
			Name: e.Sender.Name,
		},

		Room: a.chooseRoomFromEvent(e),
		Text: html.UnescapeString(htmlutil.StripTags(e.Message)),
	}
}

func (a *adapter) chooseRoomFromEvent(e *gumble.TextMessageEvent) string {
	if len(e.Channels) > 0 {
		return e.Channels[0].Name
	}

	if len(e.Trees) > 0 {
		return e.Trees[0].Name
	}

	return "(UNKNOWN)"
}

func (a *adapter) setSelfAvatar() {
	data, err := Asset("store/avatar.png")
	if err != nil {
		return
	}

	a.client.Self.SetTexture(data)
}

func (a *adapter) ttsGoRoutine() {
	stream := gumble_ffmpeg.New(a.client)
	defer stream.Stop()

	for req := range a.speakChan {
		hal.Logger.Debugf("mumble: received TTS request: %#v", req)
		a.handleSpeakRequest(stream, req)
	}
}

func (a *adapter) handleSpeakRequest(stream *gumble_ffmpeg.Stream, req ttsRequest) {
	defer func() {
		if req.Response != nil {
			close(req.Response)
		}
	}()

	source, err := a.getFestivalSource(req.Text)
	if err != nil {
		req.Response <- err
		return
	}

	stream.Source = source
	if err := stream.Play(); err != nil {
		req.Response <- err
		return
	}

	hal.Logger.Debugf("mumble: speaking: %q", req.Text)
	stream.Wait()
	hal.Logger.Debugf("mumble: done speaking: %q", req.Text)
}

func (a *adapter) getFestivalSource(str string) (gumble_ffmpeg.Source, error) {
	cmd := exec.Command("text2wave", "-f", "16000", "-scale", "1.2", "-eval", "(voice_nitech_us_slt_arctic_hts)")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	_, err = io.WriteString(stdin, str)
	if err != nil {
		return nil, err
	}

	return gumble_ffmpeg.SourceReader(&festivalSource{
		ReadCloser: stdout,
		cmd:        cmd,
	}), nil
}

type festivalSource struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (s *festivalSource) Close() error {
	s.cmd.Process.Kill()
	s.cmd.Wait()

	return s.ReadCloser.Close()
}
