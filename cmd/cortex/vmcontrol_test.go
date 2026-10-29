package main

import (
	"strings"
	"testing"
)

func TestResolveControlConfig(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name    string
		getenv  func(string) string
		wantErr string // "" = no error; matched as a substring
		wantOn  bool
	}{
		{
			name:   "unset socket: off, tenancy not even read",
			getenv: env(envTenancy, "multi"),
		},
		{
			name:   "single tenancy",
			getenv: env(envControlSock, "/run/c.sock", envControlUIDs, "501", envTenancy, "single"),
			wantOn: true,
		},
		{
			name:   "tenancy unset",
			getenv: env(envControlSock, "/run/c.sock", envControlUIDs, "0"),
			wantOn: true,
		},
		{
			name:    "multi tenancy is a boot failure naming the variable",
			getenv:  env(envControlSock, "/run/c.sock", envControlUIDs, "0", envTenancy, "multi"),
			wantErr: envTenancy,
		},
		{
			name:    "unknown tenancy value names the variable",
			getenv:  env(envControlSock, "/run/c.sock", envControlUIDs, "0", envTenancy, "dual"),
			wantErr: envTenancy,
		},
		{
			name:    "socket without uids is a misconfiguration",
			getenv:  env(envControlSock, "/run/c.sock"),
			wantErr: envControlUIDs,
		},
		{
			name:    "a non-integer uid names the variable",
			getenv:  env(envControlSock, "/run/c.sock", envControlUIDs, "0,root"),
			wantErr: envControlUIDs,
		},
		{
			name:    "a negative uid is refused",
			getenv:  env(envControlSock, "/run/c.sock", envControlUIDs, "-1"),
			wantErr: envControlUIDs,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc, err := resolveControlConfig(tc.getenv)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if (cc.Socket != "") != tc.wantOn {
					t.Errorf("on = %v (socket %q), want %v", cc.Socket != "", cc.Socket, tc.wantOn)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error naming %s, got config %+v", tc.wantErr, cc)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name %s", err, tc.wantErr)
			}
		})
	}

	// The uid list parses comma-separated with whitespace.
	cc, err := resolveControlConfig(env(envControlSock, "/run/c.sock", envControlUIDs, "0, 501 ,2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.AllowedUIDs) != 3 || cc.AllowedUIDs[0] != 0 || cc.AllowedUIDs[1] != 501 || cc.AllowedUIDs[2] != 2 {
		t.Errorf("uids = %v, want [0 501 2]", cc.AllowedUIDs)
	}
}
