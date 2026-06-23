package hostregistry

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// MockRunner is a scripted Runner for tests: it records every Local/SSH call in
// order and returns canned replies. Local replies key on the joined "name args";
// SSH replies key on a remote-command substring so a test can script by intent
// (e.g. "command -v reposync"). It is exported so other modules importing
// hostregistry can drive the same boundary in their own tests.
type MockRunner struct {
	mu       sync.Mutex
	calls    []MockCall
	localOn  map[string]mockReply
	sshOn    []sshRule
	sshDef   mockReply
	hasSSHDe bool
}

// MockCall is one recorded invocation against a MockRunner.
type MockCall struct {
	Kind   string // "local" or "ssh"
	Target string // ssh target, or "" for local
	Cmd    string // ssh remote command, or "name arg arg" for local
}

type mockReply struct {
	out string
	err error
}

type sshRule struct {
	contains string
	reply    mockReply
}

// NewMockRunner returns a MockRunner with no scripted replies.
func NewMockRunner() *MockRunner {
	return &MockRunner{localOn: map[string]mockReply{}}
}

// OnLocal scripts the reply for a Local call whose joined "name args" equals key.
func (m *MockRunner) OnLocal(key, out string, err error) *MockRunner {
	m.localOn[key] = mockReply{out: out, err: err}
	return m
}

// OnSSH scripts the reply for any SSH call whose remote command contains the
// given substring; rules are matched in registration order.
func (m *MockRunner) OnSSH(contains, out string, err error) *MockRunner {
	m.sshOn = append(m.sshOn, sshRule{contains: contains, reply: mockReply{out: out, err: err}})
	return m
}

// DefaultSSH sets the reply returned for an SSH call that matches no OnSSH rule.
func (m *MockRunner) DefaultSSH(out string, err error) *MockRunner {
	m.sshDef = mockReply{out: out, err: err}
	m.hasSSHDe = true
	return m
}

func (m *MockRunner) Local(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	m.mu.Lock()
	m.calls = append(m.calls, MockCall{Kind: "local", Cmd: key})
	r, ok := m.localOn[key]
	m.mu.Unlock()
	if !ok {
		return "", errors.New("unscripted local: " + key)
	}
	return r.out, r.err
}

func (m *MockRunner) SSH(_ context.Context, target, remoteCmd string) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, MockCall{Kind: "ssh", Target: target, Cmd: remoteCmd})
	m.mu.Unlock()
	for _, rule := range m.sshOn {
		if strings.Contains(remoteCmd, rule.contains) {
			return rule.reply.out, rule.reply.err
		}
	}
	if m.hasSSHDe {
		return m.sshDef.out, m.sshDef.err
	}
	return "", errors.New("unscripted ssh: " + remoteCmd)
}

// SSHCmds returns, in order, every SSH remote command recorded against target.
func (m *MockRunner) SSHCmds(target string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if c.Kind == "ssh" && c.Target == target {
			out = append(out, c.Cmd)
		}
	}
	return out
}

// SSHCmdsAll returns, in order, every SSH remote command recorded against any target.
func (m *MockRunner) SSHCmdsAll() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if c.Kind == "ssh" {
			out = append(out, c.Cmd)
		}
	}
	return out
}
