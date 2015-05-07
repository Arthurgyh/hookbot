package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codegangsta/cli"
	"golang.org/x/net/websocket"
)

func main() {
	app := cli.NewApp()
	app.Name = "hookbot"
	app.Usage = "turn webhooks into websockets"

	app.Commands = []cli.Command{
		{
			Name:   "serve",
			Usage:  "start a hookbot instance, listening on http",
			Action: ActionServe,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "bind, b",
					Value: ":8080",
					Usage: "address to listen on",
				},
			},
		},
		{
			Name:  "make-token",
			Usage: "given a URI, generate a token",
			Action: func(c *cli.Context) {
				key, _ := MustGetKeysFromEnv()
				if len(c.Args()) != 1 {
					cli.ShowSubcommandHelp(c)
					os.Exit(1)
				}

				url := c.Args().First()
				fmt.Println(Sha1HMAC(key, url))
			},
		},
	}

	app.RunAndExitOnError()
}

func MustGetKeysFromEnv() (string, string) {
	var (
		key           = os.Getenv("HOOKBOT_KEY")
		github_secret = os.Getenv("HOOKBOT_GITHUB_SECRET")
	)

	if key == "" || github_secret == "" {
		log.Fatalln("Error: HOOKBOT_KEY or HOOKBOT_GITHUB_SECRET not set")
	}

	return key, github_secret
}

func ActionServe(c *cli.Context) {
	key, github_secret := MustGetKeysFromEnv()

	hookbot := NewHookbot(key, github_secret)
	http.Handle("/", hookbot)
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	log.Println("Listening on", c.String("bind"))
	err := http.ListenAndServe(c.String("bind"), nil)
	if err != nil {
		log.Fatal(err)
	}
}

type Message struct {
	Topic string
	Body  []byte
	Done  chan struct{} // signalled when messages have been strobed
	// (this is not the same as when they have been received)
}

type Listener struct {
	Topic string
	c     chan []byte
}

type Hookbot struct {
	key, github_secret string

	wg       *sync.WaitGroup
	shutdown chan struct{}

	http.Handler

	message                  chan Message
	addListener, delListener chan Listener
}

func NewHookbot(key, github_secret string) *Hookbot {
	h := &Hookbot{
		key: key, github_secret: github_secret,

		wg:       &sync.WaitGroup{},
		shutdown: make(chan struct{}),

		message:     make(chan Message, 1),
		addListener: make(chan Listener, 1),
		delListener: make(chan Listener, 1),
	}

	mux := http.NewServeMux()
	mux.Handle("/sub/", websocket.Handler(h.ServeSubscribe))
	mux.HandleFunc("/pub/", h.ServePublish)

	// Middlewares
	h.Handler = mux
	h.Handler = h.KeyChecker(h.Handler)

	h.wg.Add(1)
	go h.Loop()

	return h
}

var timeout = 1 * time.Second

func TimeoutSend(wg *sync.WaitGroup, c chan []byte, m []byte) {
	defer wg.Done()

	select {
	case c <- m:
	case <-time.After(timeout):
	}
}

// Shut down main loop and wait for all in-flight messages to send or timeout
func (h *Hookbot) Shutdown() {
	close(h.shutdown)
	h.wg.Wait()
}

// Manage fanout from h.message onto listeners
func (h *Hookbot) Loop() {
	defer h.wg.Done()

	listeners := map[Listener]struct{}{}

	for {
		select {
		case m := <-h.message:

			// Strobe all interested listeners
			for listener := range listeners {
				if listener.Topic == m.Topic {
					h.wg.Add(1)
					go TimeoutSend(h.wg, listener.c, m.Body)
				}
			}

			close(m.Done)

		case l := <-h.addListener:
			listeners[l] = struct{}{}
		case l := <-h.delListener:
			delete(listeners, l)
		case <-h.shutdown:
			return
		}
	}
}

func (h *Hookbot) Add(topic string) Listener {
	l := Listener{Topic: topic, c: make(chan []byte, 1)}
	h.addListener <- l
	return l
}

func (h *Hookbot) Del(l Listener) {
	h.delListener <- l
}

func SecureEqual(x, y string) bool {
	if subtle.ConstantTimeCompare([]byte(x), []byte(y)) == 1 {
		return true
	}
	return false
}

func (h *Hookbot) IsGithubKeyOK(w http.ResponseWriter, r *http.Request) bool {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Not Authorized", http.StatusUnauthorized)
	}

	r.Body = ioutil.NopCloser(bytes.NewReader(body))

	expected := fmt.Sprintf("sha1=%v", Sha1HMAC(h.github_secret, string(body)))

	return SecureEqual(r.Header.Get("X-Hub-Signature"), expected)
}

func Sha1HMAC(key, payload string) string {
	mac := hmac.New(sha1.New, []byte(key))
	_, _ = mac.Write([]byte(payload))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func (h *Hookbot) IsKeyOK(w http.ResponseWriter, r *http.Request) bool {

	if _, ok := r.Header["X-Hub-Signature"]; ok {
		if !h.IsGithubKeyOK(w, r) {
			return false
		}
		return true
	}

	authorization := r.Header.Get("Authorization")
	fields := strings.Fields(authorization)

	if len(fields) != 2 {
		return false
	}

	authType, givenKey := fields[0], fields[1]

	var givenMac string

	switch strings.ToLower(authType) {
	default:
		return false // Not understood
	case "basic":
		givenMacBytes, err := base64.StdEncoding.DecodeString(givenKey)
		if err != nil {
			return false
		}
		// Remove trailing right colon, since it should be blank.
		givenMac = strings.TrimRight(string(givenMacBytes), ":")

	case "bearer":
		givenMac = givenKey // No processing required
	}

	expectedMac := Sha1HMAC(h.key, r.URL.Path)

	if !SecureEqual(givenMac, expectedMac) {
		return false
	}

	return true
}

func (h *Hookbot) KeyChecker(wrapped http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.IsKeyOK(w, r) {
			http.NotFound(w, r)
			return
		}

		wrapped.ServeHTTP(w, r)
	}
}

// The topic is everything after the "/pub/" or "/sub/"
var TopicRE *regexp.Regexp = regexp.MustCompile("/[^/]+/(.*)")

func Topic(url *url.URL) string {
	m := TopicRE.FindStringSubmatch(url.Path)
	if m == nil {
		return ""
	}
	return m[1]
}

func (h *Hookbot) ServePublish(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		log.Printf("Error serving %v: %v", r.URL, err)
		return
	}

	topic := Topic(r.URL)
	h.message <- Message{Topic: topic, Body: body}
	fmt.Fprintln(w, "OK")
}

func (h *Hookbot) ServeSubscribe(conn *websocket.Conn) {

	topic := Topic(conn.Request().URL)

	listener := h.Add(topic)
	defer h.Del(listener)

	closed := make(chan struct{})

	go func() {
		defer close(closed)
		_, _ = io.Copy(ioutil.Discard, conn)
	}()

	var message []byte

	for {
		select {
		case message = <-listener.c:
		case <-closed:
			log.Printf("Client disconnected")
			return
		}

		conn.SetWriteDeadline(time.Now().Add(90 * time.Second))
		n, err := conn.Write(message)
		switch {
		case n != len(message):
			log.Printf("Short write %d != %d", n, len(message))
			return // short write
		case err == io.EOF:
			return // done
		case err != nil:
			log.Printf("Error in conn.Write: %v", err)
			return // unknown error
		}
	}
}