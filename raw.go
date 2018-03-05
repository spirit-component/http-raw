package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/go-spirit/spirit/component"
	"github.com/go-spirit/spirit/doc"
	"github.com/go-spirit/spirit/mail"
	"github.com/go-spirit/spirit/message"
	"github.com/go-spirit/spirit/worker"
	"github.com/go-spirit/spirit/worker/fbp"
	"github.com/go-spirit/spirit/worker/fbp/protocol"
)

type HTTPRawComponent struct {
	opts  component.Options
	alias string

	router *gin.Engine
}

type ctxHttpComponentKey struct{}

type httpCacheItem struct {
	c    *gin.Context
	ctx  context.Context
	done chan struct{}
}

func init() {
	component.RegisterComponent("http-raw", NewHTTPRawComponent)
	doc.RegisterDocumenter("http-raw", &HTTPRawComponent{})

}

func NewHTTPRawComponent(alias string, opts ...component.Option) (srv component.Component, err error) {
	s := &HTTPRawComponent{
		alias: alias,
	}

	s.init(opts...)

	srv = s

	return
}

func (p *HTTPRawComponent) init(opts ...component.Option) {

	for _, o := range opts {
		o(&p.opts)
	}

	debug := p.opts.Config.GetBoolean("debug", false)
	if !debug {
		gin.SetMode("release")
	}

	rootPath := p.opts.Config.GetString("path", "/")

	router := gin.New()
	router.Use(gin.Recovery())
	router.POST(path.Join(rootPath, "message"), p.serve)

	p.router = router

	return
}

func (p *HTTPRawComponent) serve(c *gin.Context) {

	var err error

	defer func() {
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
		}
	}()

	strWait := c.DefaultQuery("wait", "false")
	strTimeout := c.DefaultQuery("timeout", "0s")

	wait, err := strconv.ParseBool(strWait)
	if err != nil {
		return
	}

	timeout, err := time.ParseDuration(strTimeout)
	if err != nil {
		return
	}

	payload := &protocol.Payload{}

	err = c.ShouldBind(payload)

	if err != nil {
		return
	}

	graph, exist := payload.GetGraph(payload.GetCurrentGraph())
	if !exist {
		err = fmt.Errorf("could not get graph of %s in HTTPRawComponent.serve", payload.GetCurrentGraph())
		return
	}

	port, err := graph.CurrentPort()

	if err != nil {
		return
	}

	session := mail.NewSession()

	session.WithPayload(payload)
	session.WithFromTo("", port.GetUrl())

	fbp.SessionWithPort(session, port.GetGraphName(), port.GetUrl(), port.GetMetadata())

	var ctx context.Context
	var cancel context.CancelFunc
	var doneChan chan struct{}

	if wait {

		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()

		doneChan = make(chan struct{})
		defer close(doneChan)

		session.WithValue(ctxHttpComponentKey{}, &httpCacheItem{c, ctx, doneChan})

	} else {
		session.WithValue(ctxHttpComponentKey{}, &httpCacheItem{c, nil, nil})
	}

	err = p.opts.Postman.Post(
		message.NewUserMessage(session),
	)

	if err != nil {
		logrus.WithField("component", "http-raw").WithField("alias", p.alias).WithError(err).Errorln("post user message failure")
		return
	}

	if !wait {
		c.JSON(http.StatusOK, struct{}{})
		return
	}

	for {
		select {
		case <-doneChan:
			{
				return
			}
		case <-ctx.Done():
			{
				c.JSON(http.StatusRequestTimeout, gin.H{"message": "request timeout"})
				return
			}
		}
	}

}

func (p *HTTPRawComponent) sendMessage(session mail.Session) (err error) {
	fbp.BreakSession(session)

	item, ok := session.Value(ctxHttpComponentKey{}).(*httpCacheItem)
	if !ok {
		err = errors.New("http component handler could not get response object")
		return
	}

	if item.done == nil || item.ctx == nil {
		return
	}

	payload, ok := session.Payload().Interface().(*protocol.Payload)
	if !ok {
		err = errors.New("could not convert session payload to *protocol.Payload")
		return
	}

	if item.ctx.Err() != nil {
		return
	}

	graph, exist := payload.GetGraph(payload.GetCurrentGraph())
	if !exist {
		err = fmt.Errorf("could not get graph of %s in HTTPRawComponent.sendMessage", payload.GetCurrentGraph())
		return
	}

	// the next reciver will process the next port
	graph.MoveForward()

	item.c.JSON(http.StatusOK, payload)

	item.done <- struct{}{}

	return
}

// It is a send out func
func (p *HTTPRawComponent) Route(session mail.Session) worker.HandlerFunc {
	return p.sendMessage
}

func (p *HTTPRawComponent) Start() error {
	go p.router.Run()
	return nil
}

func (p *HTTPRawComponent) Stop() error {
	return nil
}

func (p *HTTPRawComponent) Alias() string {
	if p == nil {
		return ""
	}
	return p.alias
}

func (p *HTTPRawComponent) Document() doc.Document {
	document := doc.Document{
		Title: "Raw HTTP message service",
		Description: `we could send protocol.Payload to this http service for internal use,
		there are two params for url query, [wait=false] and [timeout=0s], if the request graph
		where resend the response to this component handler, you should let wait=true and timeout > 0s,
		then you will get http response`,
	}

	return document
}
