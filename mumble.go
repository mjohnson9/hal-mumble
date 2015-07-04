package mumble

import (
	"errors"
	"html"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/danryan/env"
	"github.com/layeh/gumble/gumble"
	"github.com/layeh/gumble/gumble_ffmpeg"
	"github.com/layeh/gumble/gumbleutil"
	_ "github.com/layeh/gumble/opus"
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
	config := gumble.NewConfig()

	config.Address = net.JoinHostPort(a.server, strconv.Itoa(a.port))
	config.Username = a.Robot.Name
	config.Password = a.password
	config.TLSConfig.InsecureSkipVerify = true
	config.AudioInterval = 10 * time.Millisecond

	conn := gumble.NewClient(config)

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

	a.client = conn

	err := conn.Connect()
	if err != nil {
		return err
	}

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

	stream.Volume = 0.25

	for req := range a.speakChan {
		a.handleSpeakRequest(stream, req)
	}
}

func (a *adapter) handleSpeakRequest(stream *gumble_ffmpeg.Stream, req ttsRequest) {
	defer func() {
		if req.Response != nil {
			close(req.Response)
		}
	}()

	source, err := a.getTTSSource(req.Text)
	if err != nil {
		req.Response <- err
		return
	}

	stream.Source = source
	if err := stream.Play(); err != nil {
		req.Response <- err
		return
	}

	stream.Wait()
}

func (a *adapter) getTTSSource(str string) (gumble_ffmpeg.Source, error) {
	return gumble_ffmpeg.SourceExec("picospeaker", "--type", "wav", "--output", "-", str), nil
	//return &festivalSource{str: str}, nil
}

/*type festivalSource struct {
	str string

	cmd *exec.Cmd
}

func (*festivalSource) Arguments() []string {
	return []string{"-i", "-"}
}

func (s *festivalSource) Start(cmd *exec.Cmd) error {
	s.cmd = exec.Command("text2wave", "-f", "16000", "-scale", "1.2", "-eval", "(voice_nitech_us_slt_arctic_hts)")

	r, err := s.cmd.StdoutPipe()
	if err != nil {
		return err
	}

	cmd.Stdin = r

	stdin, err := s.cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	if err := s.cmd.Start(); err != nil {
		cmd.Stdin = nil
		return err
	}

	_, err = io.WriteString(stdin, s.str)
	if err != nil {
		return err
	}

	return nil
}

func (s *festivalSource) Done() {
	if s.cmd != nil {
		if p := s.cmd.Process; p != nil {
			p.Kill()
		}
		s.cmd.Wait()
	}
}*/
