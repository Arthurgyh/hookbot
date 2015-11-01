package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"regexp"

	"github.com/codegangsta/cli"

	"github.com/scraperwiki/hookbot/pkg/hookbot"
	"github.com/scraperwiki/hookbot/pkg/router/github"
)

func main() {
	app := cli.NewApp()
	app.Name = "hookbot"
	app.Usage = "turn webhooks into websockets"
	app.Version = Version

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "key",
			Usage:  "secret known only for hookbot for URL access control",
			Value:  "<unset>",
			EnvVar: "HOOKBOT_KEY",
		},
		cli.StringFlag{
			Name:   "github-secret",
			Usage:  "secret known by github for signing messages",
			Value:  "<unset>",
			EnvVar: "HOOKBOT_GITHUB_SECRET",
		},
	}

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
				cli.StringSliceFlag{
					Name:  "router",
					Value: &cli.StringSlice{},
					Usage: "list of routers to enable",
				},
			},
		},
		{
			Name:    "make-tokens",
			Aliases: []string{"t"},
			Usage:   "given a list of URIs, generate tokens one per line",
			Action:  ActionMakeTokens,
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "bare",
					Usage: "print only tokens (not as basic-auth URLs)",
				},
				cli.StringFlag{
					Name:   "url-base, U",
					Value:  "http://localhost:8080",
					Usage:  "base URL to generate for (not included in hmac)",
					EnvVar: "HOOKBOT_URL_BASE",
				},
			},
		},
		{
			Name:   "route-github",
			Usage:  "route github requests",
			Action: github.ActionRoute,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "monitor-url, u",
					Usage: "URL to monitor",
				},
				cli.StringFlag{
					Name:   "origin",
					Value:  "samehost",
					Usage:  "URL to use for the origin header ('samehost' is special)",
					EnvVar: "HOOKBOT_ORIGIN",
				},
				cli.StringSliceFlag{
					Name:   "header, H",
					Usage:  "headers to pass to the remote",
					Value:  &cli.StringSlice{},
					EnvVar: "HOOKBOT_HEADER",
				},
			},
		},
	}

	app.RunAndExitOnError()
}

var SubscribeURIRE = regexp.MustCompile("^(?:/unsafe)?/sub")

func ActionMakeTokens(c *cli.Context) {
	key := c.GlobalString("key")
	if key == "<unset>" {
		log.Fatalln("HOOKBOT_KEY not set")
	}

	if len(c.Args()) < 1 {
		cli.ShowSubcommandHelp(c)
		os.Exit(1)
	}

	baseStr := c.String("url-base")
	u, err := url.ParseRequestURI(baseStr)
	if err != nil {
		log.Fatal("Unable to parse url-base %q: %v", baseStr, err)
	}

	initialScheme := u.Scheme

	getScheme := func(target string) string {

		scheme := "http"

		secure := "" // if https or wss, "s", "" otherwise.
		switch initialScheme {
		case "https", "wss":
			secure = "s"
		}

		// If it's pub, use http(s), sub ws(s)
		if SubscribeURIRE.MatchString(target) {
			scheme = "ws"
		}
		return scheme + secure
	}

	for _, arg := range c.Args() {
		mac := hookbot.Sha1HMAC(key, arg)
		if c.Bool("bare") {
			fmt.Println(mac)
		} else {
			u.Scheme = getScheme(arg)
			u.User = url.User(mac)
			u.Path = arg
			fmt.Println(u)
		}
	}
}

func ActionServe(c *cli.Context) {
	key := c.GlobalString("key")
	if key == "<unset>" {
		log.Fatalln("HOOKBOT_KEY not set")
	}

	hb := hookbot.New(key)

	// Setup routers configured on the command line
	hookbot.ConfigureRouters(c, hb)

	http.Handle("/", hb)
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	log.Println("Listening on", c.String("bind"))
	err := http.ListenAndServe(c.String("bind"), nil)
	if err != nil {
		log.Fatal(err)
	}
}
