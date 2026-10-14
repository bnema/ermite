// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"bnema.dev/ermite/internal/ports"
)

// hookDialTimeout bounds connecting to the socket and the whole exchange.
const (
	hookDialTimeout = 5 * time.Second
	hookCallTimeout = 2 * time.Minute
)

// ErrHookRejected is returned by RunHook when ermite refuses the push.
var ErrHookRejected = errors.New("push refused")

// RunHook is `ermite hook <hook>`: it reads the ref updates git feeds a
// receive hook ("<old> <new> <ref>" per line) from in, sends them with the
// token to ermite over the socket, and writes ermite's message to errOut.
//
// For pre-receive it fails closed: a missing socket or token, an unreachable
// or erroring server, or a refusal all return an error (git then rejects the
// push). For post-receive the push already happened: failures are reported on
// errOut and RunHook returns nil.
func RunHook(ctx context.Context, hook string, in io.Reader, errOut io.Writer, socket, token string, objects ports.PushObjects) error {
	err := callHook(ctx, hook, in, errOut, socket, token, objects)
	if err == nil {
		return nil
	}
	if hook == HookPostReceive {
		_, _ = fmt.Fprintf(errOut, "ermite: %s: %v\n", hook, err)
		return nil
	}
	return err
}

func callHook(ctx context.Context, hook string, in io.Reader, errOut io.Writer, socket, token string, objects ports.PushObjects) error {
	if hook != HookPreReceive && hook != HookPostReceive {
		return fmt.Errorf("unknown hook %q", hook)
	}
	if socket == "" || token == "" {
		return fmt.Errorf("%w: %s and %s are not set (is this push going through ermite?)", ErrHookRejected, EnvHookSocket, EnvHookToken)
	}
	updates, err := readUpdates(in)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHookRejected, err)
	}

	ctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()
	d := net.Dialer{Timeout: hookDialTimeout}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("%w: ermite is unreachable: %v", ErrHookRejected, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req, err := encodeLine(HookRequest{Token: token, Hook: hook, Updates: updates, ObjectsDir: objects.Dir, Alternates: objects.Alternates})
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("%w: send to ermite: %v", ErrHookRejected, err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("%w: no reply from ermite: %v", ErrHookRejected, err)
	}
	var reply HookReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("%w: malformed reply from ermite", ErrHookRejected)
	}
	if reply.Message != "" {
		_, _ = fmt.Fprintln(errOut, reply.Message)
	}
	if !reply.OK {
		return ErrHookRejected
	}
	return nil
}

// readUpdates parses the lines git writes to a receive hook.
func readUpdates(in io.Reader) ([]HookUpdate, error) {
	var out []HookUpdate
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.SplitN(line, " ", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected hook input %q", line)
		}
		out = append(out, HookUpdate{Old: f[0], New: f[1], Ref: f[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read hook input: %w", err)
	}
	return out, nil
}
