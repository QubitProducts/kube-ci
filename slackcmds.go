package kubeci

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/mattn/go-shellwords"
	"github.com/slack-go/slack"
)

type slackCmds struct {
	signingSecret string
	client        *slack.Client

	ciFilePath string
	templates  TemplateSet
}

func (scs *slackCmds) setupCmd(org, repo string, args []string) *slack.Msg {
	if len(args) > 2 {
		msg := "bad wrguments: /ci-setup org/repo template [branch]"
		return &slack.Msg{
			Text: strings.Join(args, msg),
		}
	}

	if len(args) == 0 {
		msg := fmt.Sprintf("You must select a template:\n%s", scs.templates.Help())
		return &slack.Msg{
			Text: strings.Join(args, msg),
		}
	}

	branch := "master"
	if len(args) == 2 {
		branch = args[2]
	}

	return &slack.Msg{
		Text: fmt.Sprintf("TODO push %s to %s/%s (branch: %s)", scs.ciFilePath, org, repo, branch),
	}
}

func NewSlack(tokenFile, signingSecretFile string, ciFilePath string, templates TemplateSet) (*slackCmds, error) {
	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("couldn't read slack token, %w", err)
	}
	signingSecret, err := ioutil.ReadFile(signingSecretFile)
	if err != nil {
		return nil, fmt.Errorf("couldn't read slack signing secret, %w", err)
	}

	cli := slack.New(strings.TrimSpace(string(token)))
	return &slackCmds{
		client:        cli,
		signingSecret: strings.TrimSpace(string(signingSecret)),
		templates:     templates,
		ciFilePath:    ciFilePath,
	}, nil
}

func (scs *slackCmds) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	verifier, err := slack.NewSecretsVerifier(r.Header, scs.signingSecret)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	r.Body = ioutil.NopCloser(io.TeeReader(r.Body, &verifier))
	s, err := slack.SlashCommandParse(r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = verifier.Ensure(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var resp *slack.Msg
	args, err := shellwords.Parse(s.Text)
	if err != nil {
		slackReply(w, &slack.Msg{Text: err.Error()})
		return
	}

	if len(args) < 1 {
		slackReply(w, &slack.Msg{Text: "need org/repo argument"})
		return
	}

	parts := strings.SplitN(args[0], "/", 2)
	if len(parts) != 2 {
		slackReply(w, &slack.Msg{Text: "need org/repo argument"})
		return
	}
	org, repo := parts[0], parts[1]
	args = args[1:]

	switch s.Command {
	case "/ci-setup":
		resp = scs.setupCmd(org, repo, args)
	case "/ci-build":
		resp = &slack.Msg{Text: s.Text}
	case "/ci-deploy":
		resp = &slack.Msg{Text: s.Text}
	default:
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	slackReply(w, resp)
}

func slackReply(w http.ResponseWriter, resp *slack.Msg) {
	b, err := json.Marshal(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}
