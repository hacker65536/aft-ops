package cli

import (
	"strings"
	"testing"

	"github.com/hacker65536/aft-ops/internal/config"
)

// A write profile that lands in another account has to be named, along with
// the read side it disagrees with: which one is wrong decides what the
// operator does next.
func TestCrossAccountWriteErrorNamesBothSides(t *testing.T) {
	err := crossAccountWriteError(
		"105154922941", "poc-read",
		"670512287696", "prod-admin",
	)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"105154922941", "poc-read", // the account being read
		"670512287696", "prod-admin", // the account about to be written
		"--write-profile", // the remedy
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}

// With no read account resolved the pair cannot be compared, so the write is
// still refused — but the message must not claim to know an account it never
// determined.
func TestCrossAccountWriteErrorWithUnknownReadAccount(t *testing.T) {
	err := crossAccountWriteError("", "(unset — using the default credential chain)",
		"670512287696", "prod-admin")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be determined") {
		t.Errorf("message should say the read account is unknown:\n%s", msg)
	}
	if !strings.Contains(msg, "670512287696") {
		t.Errorf("message should still name the write account:\n%s", msg)
	}
}

// changedSet builds the flag predicate rootFlags.apply takes.
func changedSet(names ...string) func(string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

// --profile moves the read profile and nothing else. A configured
// write_profile survives it, which is what makes the account check in
// WriteAWS the thing standing between a redirected read and a write that
// would still land in the configured account.
func TestProfileFlagLeavesWriteProfileAlone(t *testing.T) {
	cfg := config.Config{Profile: "prod-read", WriteProfile: "prod-admin"}
	f := rootFlags{profile: "poc-admin", writeProfile: "ignored-unless-passed"}

	f.apply(changedSet("profile"), &cfg)

	if cfg.Profile != "poc-admin" {
		t.Errorf("profile = %q, want poc-admin", cfg.Profile)
	}
	if cfg.WriteProfile != "prod-admin" {
		t.Errorf("write_profile = %q, want the configured prod-admin", cfg.WriteProfile)
	}
}

// --write-profile is the way to move both sides in one run.
func TestWriteProfileFlagOverridesConfig(t *testing.T) {
	cfg := config.Config{Profile: "prod-read", WriteProfile: "prod-admin"}
	f := rootFlags{profile: "poc-admin", writeProfile: "poc-admin"}

	f.apply(changedSet("profile", "write-profile"), &cfg)

	if cfg.Profile != "poc-admin" || cfg.WriteProfile != "poc-admin" {
		t.Errorf("profiles = %q/%q, want both poc-admin", cfg.Profile, cfg.WriteProfile)
	}
	if got := cfg.EffectiveWriteProfile(); got != "poc-admin" {
		t.Errorf("EffectiveWriteProfile() = %q, want poc-admin", got)
	}
}

// An unset flag never overwrites a configured value: the whole merge order
// rests on this.
func TestUnpassedFlagsLeaveConfigUntouched(t *testing.T) {
	cfg := config.Config{Profile: "prod-read", WriteProfile: "prod-admin", Region: "ap-northeast-1"}
	cfg.Batch.Concurrency = 10
	cfg.Batch.RPS = 8
	want := cfg

	rootFlags{}.apply(changedSet(), &cfg)

	if cfg != want {
		t.Errorf("config changed with no flags passed:\ngot  %+v\nwant %+v", cfg, want)
	}
}
