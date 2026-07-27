package cli

import "testing"

func TestParseCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want Options
	}{
		{name: "default help", want: Options{Action: ActionHelp}},
		{name: "short help", args: []string{"-h"}, want: Options{Action: ActionHelp}},
		{name: "word help", args: []string{"help"}, want: Options{Action: ActionHelp}},
		{name: "configure", args: []string{"-cf"}, want: Options{Action: ActionConfigure}},
		{name: "setup", args: []string{"--setup"}, want: Options{Action: ActionSetup}},
		{name: "tunnel", args: []string{"-p", "3000"}, want: Options{Action: ActionTunnel, Port: 3000}},
		{
			name: "tunnel long",
			args: []string{"--host", "127.0.0.2", "--port", "8080"},
			want: Options{Action: ActionTunnel, Port: 8080, Host: "127.0.0.2"},
		},
		{
			name: "internal server",
			args: []string{"--server", "--server-config", "/tmp/server.json"},
			want: Options{Action: ActionServer, ServerConfig: "/tmp/server.json"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.args)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("Parse() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseRejectsConflicts(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--setup", "-p", "3000"},
		{"-p", "0"},
		{"-p", "70000"},
		{"-p", "3000", "--port", "4000"},
		{"--config", "--host", "localhost"},
		{"--dry-run"},
		{"--setup", "--setup-root", "/tmp/preview"},
		{"--wat"},
		{"unexpected"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", args)
		}
	}
}
