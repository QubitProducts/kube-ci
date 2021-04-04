package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/mattn/go-shellwords"
	"github.com/slack-go/slack"
)

type slackCheckRunMessage struct {
	client *slack.Client
	ch     string
	ts     string

	crID       int64
	title      string
	msg        string
	status     string
	conclusion string
}

func (scrm *slackCheckRunMessage) ToMessage() []slack.MsgOption {
	return []slack.MsgOption{}
}

func (scrm *slackCheckRunMessage) StatusUpdate(
	ctx context.Context,
	crID int64,
	title string,
	msg string,
	status string,
	conclusion string,
) {
	scrm.crID = crID
	scrm.title = title
	scrm.msg = msg
	scrm.status = status
	scrm.conclusion = conclusion

	opts := scrm.ToMessage()
	scrm.client.UpdateMessage(scrm.ch, scrm.ts, opts...)
}

type slackCmds struct {
	signingSecret string
	client        *slack.Client
	runner        workflowRunner

	ciFilePath string
	templates  TemplateSet
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

func (scs *slackCmds) setupCmd(m slack.SlashCommand, org, repo string, args []string) *slack.Msg {
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

func (scs *slackCmds) buildCmd(m slack.SlashCommand, org, repo string, args []string) *slack.Msg {
	if len(args) > 2 {
		msg := "bad wrguments: /ci-build org/repo [branch]"
		return &slack.Msg{
			Text: strings.Join(args, msg),
		}
	}

	branch := "master"
	if len(args) == 1 {
		branch = args[0]
	}

	text := fmt.Sprintf("TODO push %s to %s/%s (branch: %s)", scs.ciFilePath, org, repo, branch)
	ch, ts, err := scs.client.PostMessage(m.ChannelID, slack.MsgOptionText(text, false))
	if err != nil {
		return nil
	}

	go func() {
	}()
	scs.client.UpdateMessage(ch, ts, slack.MsgOptionText("something else", false))

	return nil
}

func newSlack(tokenFile, signingSecretFile string, ciFilePath string, templates TemplateSet) (*slackCmds, error) {
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
	m, err := slack.SlashCommandParse(r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = verifier.Ensure(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var resp *slack.Msg
	args, err := shellwords.Parse(m.Text)
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

	switch m.Command {
	case "/ci-setup":
		resp = scs.setupCmd(m, org, repo, args)
	case "/ci-build":
		resp = scs.buildCmd(m, org, repo, args)
	case "/ci-deploy":
		resp = &slack.Msg{Text: m.Text}
	default:
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if resp != nil {
		slackReply(w, resp)
	}
}
