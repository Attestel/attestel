package main

import (
	"bufio"
	"os"
	"strings"
)

// env.go — a tiny, dependency-free .env loader (stdlib only, invariant #5). The Go services read
// configuration from the process environment (os.Getenv); this makes `cd auth && go run .` also pick
// up a local `.env` sitting next to the binary/source (e.g. auth/.env with the Google OAuth keys),
// matching the auth/.env.example the repo ships. It is a LOCAL DEV CONVENIENCE only:
//
//   - the REAL environment always wins (a value already set — by the shell or docker-compose — is
//     never overridden), so container deployments that inject env vars are unaffected;
//   - a missing .env is a no-op, so the stack still boots with ZERO config / ZERO network (invariant
//     #1). docker-compose keeps interpolating the ROOT .env as before.
//
// Supports blank lines, # comments, an optional leading `export `, and optional surrounding quotes.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env -> nothing to load
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		// Strip a single pair of matching surrounding quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // the real environment wins — never override an already-set var
		}
		_ = os.Setenv(key, val)
	}
}
