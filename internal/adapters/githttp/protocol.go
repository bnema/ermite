// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"encoding/json"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/domain"
)

// Hook names, as used by git and by `ermite hook`.
const (
	HookPreReceive  = "pre-receive"
	HookPostReceive = "post-receive"
)

// maxHookMessage bounds one line of the hook protocol, request or reply.
const maxHookMessage = 32 << 20

// HookRequest is the one JSON line a hook sends over the socket.
//
//	{"token":"<hook token>","hook":"pre-receive","updates":[{"old":"<oid>","new":"<oid>","ref":"refs/heads/main"}]}
//
// The repository is not sent: the token is bound to it.
type HookRequest struct {
	Token   string       `json:"token"`
	Hook    string       `json:"hook"`
	Updates []HookUpdate `json:"updates"`
	// ObjectsDir and Alternates are GIT_OBJECT_DIRECTORY and
	// GIT_ALTERNATE_OBJECT_DIRECTORIES of the hook: where the objects of a push
	// in quarantine are (pre-receive only).
	ObjectsDir string `json:"objects_dir,omitempty"`
	Alternates string `json:"alternates,omitempty"`
}

// HookUpdate is one ref update, as git feeds it to the hook.
type HookUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// HookReply is the one JSON line ermite answers. Message is shown to the
// pusher (on stderr of the hook, so git prefixes it with "remote:").
//
//	{"ok":true}  {"ok":false,"message":"push rejected: ..."}
type HookReply struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// refUpdates checks the request fields that do not need the token.
func (r HookRequest) refUpdates() ([]domain.RefUpdate, error) {
	switch r.Hook {
	case HookPreReceive, HookPostReceive:
	default:
		return nil, fmt.Errorf("unknown hook %q", r.Hook)
	}
	if r.Token == "" {
		return nil, errors.New("missing token")
	}
	out := make([]domain.RefUpdate, 0, len(r.Updates))
	for _, u := range r.Updates {
		up, err := domain.NewRefUpdate(u.Old, u.New, u.Ref)
		if err != nil {
			return nil, err
		}
		out = append(out, up)
	}
	return out, nil
}

func encodeLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
