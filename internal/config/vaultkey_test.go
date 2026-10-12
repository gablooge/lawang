package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/vault"
)

// prodEnv is a production environment that is valid but for what a case changes.
func prodEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
		"LAWANG_VAULT_KEY":    testVaultKey,
	}
	for k, v := range extra {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return env(m)
}

// TestAMissingVaultKeyInProductionIsAStartupError is the third "Done when" clause of item B13, at
// the layer that produces it: Load returns no configuration at all, so no role can start.
//
// The clause is also covered one layer up, at the command, by
// TestRunRefusesToStartWithoutAVaultKey in cmd/lawang: that one is what an operator sees.
func TestAMissingVaultKeyInProductionIsAStartupError(t *testing.T) {
	// Both spellings of production: unset, and named. "Unset" is the one that matters, because an
	// environment that forgot LAWANG_ENV as well must not be the lenient one.
	for _, e := range []string{"", "production"} {
		cfg, err := Load(prodEnv(map[string]string{"LAWANG_ENV": e, "LAWANG_VAULT_KEY": ""}))
		if err == nil {
			t.Fatalf("LAWANG_ENV=%q: Load accepted production with no vault key: %v", e, cfg)
		}
		if !strings.Contains(err.Error(), "LAWANG_VAULT_KEY") {
			t.Errorf("LAWANG_ENV=%q: the refusal does not name the variable: %v", e, err)
		}
		if !strings.Contains(err.Error(), "required in production") {
			t.Errorf("LAWANG_ENV=%q: the refusal does not say why: %v", e, err)
		}
		// Nothing usable comes back: a caller that ignored the error would have no key rather
		// than a key of Load's own.
		if !cfg.VaultKey.IsZero() {
			t.Errorf("LAWANG_ENV=%q: a refusal still returned a key", e)
		}
	}

	// And the control: the same environment with a key is accepted, so the refusal above is about
	// the key and not about the rest of it.
	cfg, err := Load(prodEnv(nil))
	if err != nil {
		t.Fatalf("Load of a production environment with a key: %v", err)
	}
	if cfg.VaultKey.IsZero() {
		t.Error("the key did not reach the configuration")
	}
}

// TestTheDevelopmentVaultKeyCannotBeReachedInProduction drives the spellings of LAWANG_ENV an
// operator, a manifest or a typo can produce, with no key set, and holds every one of them to:
// either Load refuses, or it is development and the key it generated says so. Nothing in between.
//
// The development convenience is the one branch in Load that invents a secret, so the question
// "can any input reach it in production" has to be answered by a list rather than by reading the
// code.
func TestTheDevelopmentVaultKeyCannotBeReachedInProduction(t *testing.T) {
	spellings := []string{
		"", "production", "PRODUCTION", "Production", " production", "production ",
		"development", "DEVELOPMENT", "Development", "DeVeLoPmEnT", " development",
		"development ", "development\n", "\tdevelopment", "dev", "develop", "develepment",
		"developmentt", "development;production", "prod", "staging", "test", "local", "ci",
		"true", "1", "0", "-", "development\x00", "développement", "開発",
		strings.Repeat("development", 10),
	}
	var generated int
	for _, spelling := range spellings {
		cfg, err := Load(prodEnv(map[string]string{"LAWANG_ENV": spelling, "LAWANG_VAULT_KEY": ""}))
		if err != nil {
			// A refusal is always an acceptable answer here, but it must be one of the two
			// refusals this environment can earn, and never a configuration.
			if !strings.Contains(err.Error(), "LAWANG_VAULT_KEY") && !strings.Contains(err.Error(), "LAWANG_ENV") {
				t.Errorf("LAWANG_ENV=%q: the refusal names neither variable: %v", spelling, err)
			}
			if !cfg.VaultKey.IsZero() {
				t.Errorf("LAWANG_ENV=%q: a refusal still returned a key", spelling)
			}
			continue
		}
		// It was accepted with no key configured, so it can only be development, and the key can
		// only be a generated one.
		if cfg.Env != Development {
			t.Errorf("LAWANG_ENV=%q: accepted as %q with no vault key", spelling, cfg.Env)
			continue
		}
		if spelling != string(Development) {
			t.Errorf("LAWANG_ENV=%q was read as development", spelling)
		}
		if cfg.VaultKey.IsZero() || !cfg.VaultKey.Generated() {
			t.Errorf("LAWANG_ENV=%q: development did not generate a key: %v", spelling, cfg.VaultKey)
		}
		generated++
	}
	// Exactly one of the spellings is development, and the loop ran over all of them: a matrix
	// that silently stopped early would otherwise report a clean result.
	if generated != 1 {
		t.Errorf("%d of %d spellings reached the development branch, want exactly one",
			generated, len(spellings))
	}
}

func TestTheDevelopmentVaultKeyIsRealAndDifferentEveryTime(t *testing.T) {
	// It has to be a key the vault can actually use, and it has to be this process's own: two
	// Loads that produced the same key would mean it came from somewhere, which is the thing
	// development is deliberately not doing.
	seen := map[string]bool{}
	for range 8 {
		cfg, err := Load(env(map[string]string{"LAWANG_ENV": "development"}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		key := cfg.VaultKey.Bytes()
		if len(key) != vault.MasterKeyLen {
			t.Fatalf("the generated key is %d bytes, want %d", len(key), vault.MasterKeyLen)
		}
		if bytes.Equal(key, make([]byte, vault.MasterKeyLen)) {
			t.Fatal("the generated key is all zeros")
		}
		if seen[string(key)] {
			t.Fatal("two Loads generated the same development key")
		}
		seen[string(key)] = true
	}
}

// TestTheVaultKeyLengthIsTheOneTheVaultRequires holds the number this package writes down against
// the package that uses it. config depends on nothing of this program, so the constant is
// duplicated on purpose, and this test is what keeps the duplicate from drifting.
func TestTheVaultKeyLengthIsTheOneTheVaultRequires(t *testing.T) {
	if masterKeyLen != vault.MasterKeyLen {
		t.Fatalf("config says the master key is %d bytes and the vault says %d",
			masterKeyLen, vault.MasterKeyLen)
	}
}

func TestTheKeyLoadCarriesIsTheOneTheVariableHeld(t *testing.T) {
	cfg, err := Load(prodEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, err := base64.StdEncoding.DecodeString(testVaultKey)
	if err != nil {
		t.Fatalf("the test key is not base64: %v", err)
	}
	got := cfg.VaultKey.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatal("the key in the configuration is not the one the variable held")
	}
	if cfg.VaultKey.Generated() {
		t.Error("a configured key is reported as generated")
	}
	// A caller that edits what Bytes handed it cannot change the configuration. (The value
	// receiver is what makes that true today; see Bytes.)
	got[0] ^= 0xFF
	if again := cfg.VaultKey.Bytes(); !bytes.Equal(again, want) {
		t.Error("editing the result of Bytes changed the configuration")
	}
	// And the zero key has no bytes at all, rather than 32 zeros that a caller could seal with.
	var zero MasterKey
	if got := zero.Bytes(); got != nil {
		t.Errorf("the zero key returned %d bytes", len(got))
	}
}

// TestTheVaultKeyIsNeverPrinted is the rule for the one secret this deployment holds for itself.
// A key in a log line turns an encrypted table back into a plain one, because the backup that
// carries the table carries the log.
func TestTheVaultKeyIsNeverPrinted(t *testing.T) {
	// A key whose base64 and whose raw bytes are both searched for: a %#v of the array would
	// print the bytes as numbers, which is why the decoded form is checked too.
	cfg, err := Load(prodEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(testVaultKey)
	if err != nil {
		t.Fatalf("the test key is not base64: %v", err)
	}
	// Both spellings Go prints a byte array in: "%v" of an array separates with a space and a
	// slice of numbers in source or in JSON separates with a comma. Only checking one of them
	// would clear a %#v that prints the array, which is the leak this test exists for.
	var spaced, commaed strings.Builder
	for i, b := range raw {
		if i > 0 {
			spaced.WriteString(" ")
			commaed.WriteString(",")
		}
		fmt.Fprintf(&spaced, "%d", b)
		fmt.Fprintf(&commaed, "%d", b)
	}
	// Every spelling of the key a printer could produce: base64, the two number forms, the raw
	// bytes as a string (what a text log of string(key) would hold), and hex. A list with only
	// one of them on it would clear four ways of leaking the key.
	asBytes := []string{
		spaced.String(),
		commaed.String(),
		string(raw),
		// What a text log or a %q prints for those bytes: the handlers escape them, so the raw
		// spelling above would not match what one actually wrote. The JSON handler is lossier
		// still (invalid UTF-8 becomes U+FFFD), and the assertion further down that every
		// printed form must say whether a key is configured is what covers that one.
		strings.Trim(strconv.Quote(string(raw)), `"`),
		hex.EncodeToString(raw),
		strings.ToUpper(hex.EncodeToString(raw)),
	}

	printed := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		printed[verb+" of the config"] = fmt.Sprintf(verb, cfg)
		printed[verb+" of a pointer to it"] = fmt.Sprintf(verb, &cfg)
		printed[verb+" of the key"] = fmt.Sprintf(verb, cfg.VaultKey)
		printed[verb+" of a pointer to the key"] = fmt.Sprintf(verb, &cfg.VaultKey)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "cfg", cfg, "key", cfg.VaultKey)
	printed["the json log"] = buf.String()
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "cfg", cfg, "key", cfg.VaultKey)
	printed["the text log"] = buf.String()

	for where, out := range printed {
		if strings.Contains(out, testVaultKey) {
			t.Errorf("%s holds the key: %s", where, out)
		}
		for _, spelling := range asBytes {
			if strings.Contains(out, spelling) {
				t.Errorf("%s holds the key as bytes: %s", where, out)
			}
		}
		// It says there is one, which is what an operator debugging a vault needs.
		if !strings.Contains(out, "configured") {
			t.Errorf("%s does not say whether a key is configured: %s", where, out)
		}
	}

	// The other two states say what they are, so that a developer can see that what they stored
	// is gone at the next restart, and an operator can see that there is no key at all.
	dev, err := Load(env(map[string]string{"LAWANG_ENV": "development"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fmt.Sprintf("%v", dev.VaultKey); !strings.Contains(got, "generated for development") {
		t.Errorf("a generated key prints as %q", got)
	}
	var zero MasterKey
	if got := fmt.Sprintf("%v", zero); !strings.Contains(got, "none") {
		t.Errorf("no key prints as %q", got)
	}
}

func TestParseMasterKeyRefusesEverythingThatIsNotOne(t *testing.T) {
	// leak is what a value pasted into this variable from the wrong place looks like. No refusal
	// may repeat it: Load cannot know which variable a secret was pasted into.
	const leak = "postgres://leakuser:hunter2@leakhost:5432/leakdb"
	for name, raw := range map[string]string{
		"empty":                    "",
		"not base64":               "x",
		"a database URL":           leak,
		"64 hex characters":        strings.Repeat("ab", 32),
		"base64 of 16 bytes":       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16)),
		"base64 of 31 bytes":       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31)),
		"base64 of 33 bytes":       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 33)),
		"unpadded base64":          strings.TrimSuffix(testVaultKey, "="),
		"url-safe base64":          base64.URLEncoding.EncodeToString(append(bytes.Repeat([]byte{0xFB}, 31), 0xFF)),
		"a trailing newline":       testVaultKey + "\n",
		"a leading space":          " " + testVaultKey,
		"a space in the middle":    testVaultKey[:20] + " " + testVaultKey[20:],
		"the key twice":            testVaultKey + testVaultKey,
		"non-canonical final bits": testVaultKey[:len(testVaultKey)-2] + "/=",
		"a sentence about the key": "the key is in the secret store",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseMasterKey(raw)
			if err == nil {
				t.Fatalf("ParseMasterKey accepted it, and returned a key of %d bytes", len(got.Bytes()))
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a key")
			}
			// Nothing of the value, and nothing of the key it would have decoded to.
			if raw != "" && strings.Contains(err.Error(), raw) {
				t.Errorf("the refusal echoes the value: %v", err)
			}
			for _, frag := range []string{"hunter2", "leakuser", "leakhost", "leakdb"} {
				if strings.Contains(err.Error(), frag) {
					t.Errorf("the refusal leaks %q: %v", frag, err)
				}
			}
			// The same value through Load names the variable, which is what an operator reads.
			if _, err := Load(prodEnv(map[string]string{"LAWANG_VAULT_KEY": raw})); err == nil {
				t.Error("Load accepted it")
			} else if !strings.Contains(err.Error(), "LAWANG_VAULT_KEY") {
				t.Errorf("Load's refusal does not name the variable: %v", err)
			}
		})
	}
}

func TestParseMasterKeyAcceptsWhatTheDocumentationSays(t *testing.T) {
	// What "openssl rand -base64 32" prints: 44 characters of standard base64, padded. The two
	// test keys are exactly that, and so is a key of 32 high bytes, whose final character is one
	// where the padding bits matter.
	for name, raw := range map[string]string{
		"the first test key":  testVaultKey,
		"the second test key": otherTestVaultKey,
		"32 high bytes":       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xFF}, 32)),
		"32 bytes of one":     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
	} {
		t.Run(name, func(t *testing.T) {
			key, err := ParseMasterKey(raw)
			if err != nil {
				t.Fatalf("ParseMasterKey: %v", err)
			}
			want, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				t.Fatalf("DecodeString: %v", err)
			}
			if !bytes.Equal(key.Bytes(), want) {
				t.Error("the parsed key is not the bytes the value decodes to")
			}
			if key.Generated() {
				t.Error("a parsed key is reported as generated")
			}
			if len(raw) != base64.StdEncoding.EncodedLen(masterKeyLen) {
				t.Errorf("the documented spelling is %d characters and this one is %d",
					base64.StdEncoding.EncodedLen(masterKeyLen), len(raw))
			}
		})
	}
	// The all-zero key parses here and the vault refuses it, which is where that rule belongs:
	// this function decides what the variable may hold, and vault.NewLocal decides what can be
	// sealed with. TestSealerRefusesAKeyThatIsNotOne is the other half.
	if _, err := ParseMasterKey(base64.StdEncoding.EncodeToString(make([]byte, masterKeyLen))); err != nil {
		t.Errorf("the all-zero key: %v, want it parsed here and refused by the vault", err)
	}
}
