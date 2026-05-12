// ircflu's web-hook subsystem.
package hooks

import (
	"fmt"
	"net/http"

	"github.com/muesli/ircflu/msgsystem"
)

var (
	Hooks []*Hook
)

type Hook interface {
	Request(w http.ResponseWriter, r *http.Request)

	SetMessageChan(channel chan msgsystem.Message)

	Name() string
	Path() string
}

func init() {
	fmt.Println("Initializing hooks subsystem...")
}

func RegisterWebHook(hook Hook) {
	fmt.Println("Registering web-hook:", hook.Name(), "on", hook.Path())

	hook.SetMessageChan(msgsystem.MessagesOut)
	Hooks = append(Hooks, &hook)

	http.HandleFunc(hook.Path(), hook.Request)
}
