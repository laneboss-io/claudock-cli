package usage_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvalidArguments(t *testing.T) {
	rows := [][]string{
		{"usage", "--names"}, {"usage", "--fresh", "--max-age", "1"}, {"usage", "--fresh", "--fresh"},
		{"usage", "--max-age"}, {"usage", "unexpected"}, {"usage", "--max-age=10"},
		{"available", "--names", "--names"}, {"available", "--fresh", "--names", "--names"},
		{"available", "--max-age", "--names", "10"}, {"available", "--names", "--fresh", "--max-age", "10"},
		{"available", "unexpected"}, {"available", "--fresh", "--fresh"},
		{"require-token"}, {"require-token", "ON"}, {"require-token", "true"}, {"require-token", "status", "extra"},
		{"version", "extra"}, {"--version", "extra"},
	}
	for _, value := range []string{"", "-1", "+1", "1.5", "1e3", " 10", "10 ", "86401", "9999999999999999999999", "１２", "١٢", "NaN"} {
		rows = append(rows, []string{"usage", "--max-age", value}, []string{"available", "--max-age", value})
	}
	for index, arguments := range rows {
		t.Run(fmt.Sprintf("row-%d", index), func(t *testing.T) {
			f := newFixture(t)
			registry := filepath.Join(f.base, "profiles.json")
			writeFile(t, registry, []byte("this registry must never be read"))
			before := readFile(t, registry)
			expect(t, f.run(arguments...), 2, "", "Run 'claudock help' for usage.")
			if !bytes.Equal(before, readFile(t, registry)) || f.requestCount() != 0 {
				t.Error("argument refusal changed files or made a remote call")
			}
			if _, err := os.Lstat(filepath.Join(f.base, "usage-cache.json")); !os.IsNotExist(err) {
				t.Error("invalid arguments created usage state")
			}
			if _, err := os.Lstat(filepath.Join(f.home, "Library/Preferences", preferenceDomain+".plist")); !os.IsNotExist(err) {
				t.Error("invalid arguments created a token preference")
			}
		})
	}
}

func TestRequireTokenRoundTrip(t *testing.T) {
	f := newFixture(t)
	preference := filepath.Join(f.home, "Library/Preferences", preferenceDomain+".plist")
	writeFile(t, preference, []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>claudeExecutable</key><string>/sample/bin/claude</string><key>otherPreference</key><dict><key>nested</key><array><string>retain</string><integer>7</integer></array></dict></dict></plist>`))
	before := readFile(t, f.store)
	expect(t, f.run("require-token", "status"), 0, "off\n", "")
	expect(t, f.run("require-token", "on"), 0, "Claudock now requires an inference token to launch subscription profiles; it no longer falls back to their normal login.\n", "")
	expect(t, f.run("require-token", "status"), 0, "on\n", "")
	data := readFile(t, preference)
	if !bytes.Contains(data, []byte("requireInferenceToken")) || !bytes.Contains(data, []byte("<true>")) || !bytes.Contains(data, []byte("/sample/bin/claude")) || !bytes.Contains(data, []byte("<integer>7</integer>")) {
		t.Error("token setting did not persist in the shared plist or discarded other preferences")
	}
	info, err := os.Stat(preference)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Error("preference was not written privately")
	}
	expect(t, f.run("require-token", "off"), 0, "Claudock no longer requires an inference token; subscription profiles without one launch with their normal login.\n", "")
	expect(t, f.run("require-token", "status"), 0, "off\n", "")
	data = readFile(t, preference)
	if bytes.Contains(data, []byte("requireInferenceToken")) || !bytes.Contains(data, []byte("otherPreference")) || !bytes.Equal(before, readFile(t, f.store)) || f.requestCount() != 0 {
		t.Error("turning the policy off failed to remove its key or touched account data")
	}
}

func TestRequireTokenMissingPreference(t *testing.T) {
	f := newFixture(t)
	expect(t, f.run("require-token", "status"), 0, "off\n", "")
	expect(t, f.run("require-token", "on"), 0, "Claudock now requires an inference token to launch subscription profiles; it no longer falls back to their normal login.\n", "")
	expect(t, f.run("require-token", "status"), 0, "on\n", "")
	if f.requestCount() != 0 {
		t.Error("policy invoked an account API")
	}
}

func TestRequireTokenRefusesUnsafePreferences(t *testing.T) {
	for _, kind := range []string{"malformed", "duplicate-key", "invalid-type", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			path := filepath.Join(f.home, "Library/Preferences", preferenceDomain+".plist")
			sentinel := filepath.Join(f.home, "preference-target")
			writeFile(t, sentinel, []byte("untouched preference canary"))
			switch kind {
			case "malformed":
				writeFile(t, path, []byte("invalid plist"))
			case "duplicate-key":
				writeFile(t, path, []byte(`<plist><dict><key>requireInferenceToken</key><true/><key>requireInferenceToken</key><false/></dict></plist>`))
			case "invalid-type":
				writeFile(t, path, []byte(`<plist><dict><key>requireInferenceToken</key><array/></dict></plist>`))
			case "symlink":
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal("could not create preference fixture directory")
				}
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal("could not create preference symlink canary")
				}
			case "directory":
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal("could not create non-file preference canary")
				}
			}
			expect(t, f.run("require-token", "status"), 1, "", "could not read the inference token requirement")
			expect(t, f.run("require-token", "on"), 1, "", "could not save the inference token requirement")
			expect(t, f.run("require-token", "off"), 1, "", "could not save the inference token requirement")
			if string(readFile(t, sentinel)) != "untouched preference canary" || f.requestCount() != 0 {
				t.Error("policy refusal changed the canary or requested a remote API")
			}
		})
	}
}

func TestCommandsRefuseUnreadableRegistry(t *testing.T) {
	for _, command := range []string{"usage", "available"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t)
			file := filepath.Join(f.base, "profiles.json")
			writeFile(t, file, []byte("invalid registry data"))
			before := readFile(t, f.store)
			expect(t, f.run(command, "--fresh"), 1, "", "Could not read the profile registry.")
			if !bytes.Equal(before, readFile(t, f.store)) || f.requestCount() != 0 {
				t.Error("registry refusal read or changed credentials, or requested an API")
			}
		})
	}
}

func TestTestBuildRefusesRemoteBases(t *testing.T) {
	for index, kind := range []string{"missing", "https", "hostname", "nonliteral-loopback", "path", "query", "userinfo"} {
		t.Run(fmt.Sprintf("row-%d", index), func(t *testing.T) {
			f := newFixture(t)
			f.add("alpha", "subscription", "pro", "")
			base := f.server.URL
			switch kind {
			case "missing":
				base = ""
			case "https":
				base = strings.Replace(base, "http://", "https://", 1)
			case "hostname":
				base = strings.Replace(base, "127.0.0.1", "localhost", 1)
			case "nonliteral-loopback":
				base = strings.Replace(base, "127.0.0.1", "127.0.0.2", 1)
			case "path":
				base += "/path"
			case "query":
				base += "?query=value"
			case "userinfo":
				base = strings.Replace(base, "http://", "http://user:password@", 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command, out, diagnostic := f.command(ctx, "usage", "--fresh")
			for i, setting := range command.Env {
				if strings.HasPrefix(setting, "CLAUDOCK_TEST_API_BASE_URL=") {
					command.Env[i] = "CLAUDOCK_TEST_API_BASE_URL=" + base
				}
			}
			if command.Start() != nil {
				t.Fatal("endpoint-refusal command could not start")
			}
			r := f.collect(command, out, diagnostic)
			f.assertNoSecrets(r)
			expect(t, r, 1, usageHeader, "The test API base must use HTTP on literal 127.0.0.1.")
			if f.requestCount() != 0 {
				t.Error("invalid test base still requested the localhost API")
			}
		})
	}
}
