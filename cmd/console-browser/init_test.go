package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerInitDropsPrivilegesBeforeTini(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"iptables", "ip6tables", "mkdir", "chown", "chmod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Observe the final exec boundary without requiring root or touching /run.
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INIT_TRACE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "setpriv"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace")
	command := exec.Command("/bin/sh", "../../deploy/console-browser/entrypoint.sh", "/bin/echo", "payload")
	command.Env = append(os.Environ(), "PATH="+dir, "INIT_TRACE="+trace)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var launched []string
	for i, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			launched = args[i:]
			break
		}
	}
	if strings.Join(launched, " ") != "/usr/bin/tini -- /bin/echo payload" {
		t.Fatalf("init must drop privilege before starting PID 1 supervisor: %q", launched)
	}
}

func TestContainerInitOwnsProfileWhileChangingMode(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "owner")
	if err := os.WriteFile(owner, []byte("65532:65532\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"iptables": "exit 0", "ip6tables": "exit 0", "mkdir": "exit 0",
		"chown": "printf '%s\\n' \"$1\" > \"$INIT_OWNER\"",
		// Root without CAP_FOWNER can chmod only while it owns the directory.
		"chmod":   "IFS= read -r owner < \"$INIT_OWNER\"\n[ \"$owner\" = '0:0' ]",
		"setpriv": "IFS= read -r owner < \"$INIT_OWNER\"\n[ \"$owner\" = '65532:65532' ]",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("/bin/sh", "../../deploy/console-browser/entrypoint.sh", "/bin/true")
	command.Env = append(os.Environ(), "PATH="+dir, "INIT_OWNER="+owner)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("profile ownership/mode ordering failed: %v %s", err, out)
	}
}
