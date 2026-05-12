// ircflu's integrated web-server to handle web-hooks.
package irc

import (
	"net/http"

	"github.com/muesli/ircflu/app"
	"github.com/muesli/ircflu/msgsystem"
)

type WebSubSystem struct {
	addr string
}

func (sys *WebSubSystem) Name() string {
	return "web"
}

func (sys *WebSubSystem) Run(channelIn, channelOut chan msgsystem.Message) {
	go http.ListenAndServe(sys.addr, nil)
}

func (sys *WebSubSystem) Handle(cm msgsystem.Message) bool {
	return false
}

func init() {
	w := WebSubSystem{}

	app.AddFlags([]app.CliFlag{
		app.CliFlag{V: &w.addr, Name: "webaddr", Value: "0.0.0.0:12346", Desc: "net.Listen spec, to listen for json-api calls"},
	})

	msgsystem.RegisterSubSystem(&w)
}
