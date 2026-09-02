package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// credential_test.go — the worker credential must not reach a Hermes child, in EITHER of its forms.
//
// THE BUG THIS COVERS. `readToken` used to unset only `ATTESTEL_WORKER_TOKEN`. The file form —
// `ATTESTEL_WORKER_TOKEN_FILE` — stayed in the environment, every stage inherited it, and an agent
// that read its own environment learned the exact path of a 0600 file holding the credential. It
// runs as the same user, so it could simply open it. The path is not the secret; it is a map to the
// secret, and the file form is the one documented as SAFER.
//
// Both halves are tested against a REAL SUBPROCESS rather than against `os.Environ()`, because the
// property that matters is what the child actually receives.

// fakeWrapper writes an executable that dumps its own environment to `dumpPath` and prints a
// minimal JSON object, then puts it on PATH under the name execRunner will look for.
func fakeWrapper(t *testing.T, profile, dumpPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake wrapper is a POSIX shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nenv > \"" + dumpPath + "\"\necho '{}'\n"
	// The name `hermes profile alias <profile>` actually creates (see profileWrapperNames).
	path := filepath.Join(dir, profile)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runOneStage invokes the real execRunner against the fake wrapper and returns the child's dumped
// environment.
func runOneStage(t *testing.T) string {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "child-env.txt")
	fakeWrapper(t, companyResearchChain[0].Profile, dump)

	workdir := t.TempDir()
	query := filepath.Join(workdir, "query.txt")
	if err := os.WriteFile(query, []byte("QUESTION"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StageBudgetSeconds: 30, MaxTurns: 4}
	if _, err := (execRunner{}).Run(context.Background(), companyResearchChain[0], workdir, query, cfg); err != nil {
		t.Fatalf("the fake wrapper did not run: %v", err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the child did not dump its environment: %v", err)
	}
	return string(raw)
}

// assertNoCredential fails if the child environment mentions either variable or the secret itself.
func assertNoCredential(t *testing.T, childEnv, secret string) {
	t.Helper()
	for _, name := range credentialEnvVars {
		if strings.Contains(childEnv, name+"=") {
			t.Fatalf("the Hermes child inherited %s", name)
		}
	}
	if secret != "" && strings.Contains(childEnv, secret) {
		t.Fatal("the Hermes child's environment contains the credential value")
	}
}

func TestTheHermesChildNeverInheritsADirectlyConfiguredToken(t *testing.T) {
	const secret = "direct-worker-token-0123456789abcdef"
	t.Setenv("ATTESTEL_WORKER_TOKEN", secret)
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", "")

	token, err := readToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != secret {
		t.Fatalf("readToken returned %q", token)
	}
	assertNoCredential(t, runOneStage(t), secret)
}

func TestTheHermesChildNeverInheritsAFileConfiguredToken(t *testing.T) {
	// THE CASE THAT WAS BROKEN. The variable holds a PATH, not the secret — and a path to a file the
	// child can open is as good as the secret.
	const secret = "file-worker-token-0123456789abcdef"
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "worker-token")
	if err := os.WriteFile(tokenPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATTESTEL_WORKER_TOKEN", "")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", tokenPath)

	token, err := readToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != secret {
		t.Fatalf("readToken returned %q", token)
	}
	childEnv := runOneStage(t)
	assertNoCredential(t, childEnv, secret)
	// And specifically not the path, which is the thing the old version leaked.
	if strings.Contains(childEnv, tokenPath) {
		t.Fatal("the Hermes child's environment contains the credential file's path")
	}
}

func TestBothCredentialVariablesAreRemovedFromThisProcess(t *testing.T) {
	// The first line of defence: an agent that reads its own environment finds nothing, because
	// there is nothing there to find.
	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"the direct form", func(t *testing.T) {
			t.Setenv("ATTESTEL_WORKER_TOKEN", "a-token")
			t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", "")
		}},
		{"the file form", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte("a-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ATTESTEL_WORKER_TOKEN", "")
			t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", path)
		}},
		{"both set at once", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte("unused"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ATTESTEL_WORKER_TOKEN", "a-token")
			t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", path)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			if _, err := readToken(); err != nil {
				t.Fatal(err)
			}
			for _, name := range credentialEnvVars {
				if v, present := os.LookupEnv(name); present {
					t.Fatalf("%s survived readToken (%q)", name, v)
				}
			}
		})
	}
}

func TestBothVariablesAreRemovedEvenWhenTheCredentialCannotBeRead(t *testing.T) {
	// A failed read must not leave the variables behind. This is why the unset is deferred rather
	// than written at each successful return.
	t.Setenv("ATTESTEL_WORKER_TOKEN", "")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := readToken(); err == nil {
		t.Fatal("a missing credential file was accepted")
	}
	for _, name := range credentialEnvVars {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("%s survived a failed readToken", name)
		}
	}
}

func TestTheSubprocessFilterHoldsEvenWithoutLoadConfig(t *testing.T) {
	// DEFENCE IN DEPTH, and the reason it is worth having: the unset in readToken protects a process
	// that went through loadConfig. This filter protects one that did not — a test, a future entry
	// point, or a caller that builds a Config itself — and would otherwise hand a live credential to
	// a model.
	const secret = "leaked-worker-token-0123456789abcdef"
	tokenPath := filepath.Join(t.TempDir(), "worker-token")
	if err := os.WriteFile(tokenPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set deliberately and DO NOT call readToken.
	t.Setenv("ATTESTEL_WORKER_TOKEN", secret)
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", tokenPath)

	childEnv := runOneStage(t)
	assertNoCredential(t, childEnv, secret)
	if strings.Contains(childEnv, tokenPath) {
		t.Fatal("the credential file's path reached the child despite the subprocess filter")
	}
}

func TestTheChildStillInheritsTheOperatorsOwnEnvironment(t *testing.T) {
	// The filter must remove credentials and NOTHING ELSE. Hermes needs the operator's environment —
	// their provider configuration lives there — so a bridge that sanitised it would break the
	// profiles it exists to run.
	t.Setenv("ATTESTEL_WORKER_TOKEN", "a-token")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", "")
	t.Setenv("SOME_OPERATOR_SETTING", "must-survive")

	childEnv := runOneStage(t)
	if !strings.Contains(childEnv, "SOME_OPERATOR_SETTING=must-survive") {
		t.Fatal("the filter removed an unrelated environment variable")
	}
	if !strings.Contains(childEnv, "PATH=") {
		t.Fatal("the child received no PATH")
	}
}

func TestEnvironWithoutMatchesOnTheNameOnly(t *testing.T) {
	// A variable whose VALUE mentions a credential name must survive: the filter removes
	// credentials, not anything that talks about one.
	in := []string{
		"ATTESTEL_WORKER_TOKEN=secret",
		"ATTESTEL_WORKER_TOKEN_FILE=/somewhere/token",
		"NOTES=remember to set ATTESTEL_WORKER_TOKEN later",
		"ATTESTEL_WORKER_TOKEN_SUFFIX=not-the-same-variable",
		"PATH=/usr/bin",
	}
	got := environWithout(in, credentialEnvVars...)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "ATTESTEL_WORKER_TOKEN=") ||
		strings.Contains(joined, "ATTESTEL_WORKER_TOKEN_FILE=") {
		t.Fatalf("a credential entry survived: %v", got)
	}
	for _, keep := range []string{"NOTES=", "ATTESTEL_WORKER_TOKEN_SUFFIX=", "PATH="} {
		if !strings.Contains(joined, keep) {
			t.Fatalf("%s was removed; the filter must match on the variable name only", keep)
		}
	}
}
