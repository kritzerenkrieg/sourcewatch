package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// needFileModes skips a test that relies on Unix file mode bits, which Windows does not have.
// The service runs on Debian (A3).
func needFileModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix file mode bits")
	}
}

func TestLoadEnv(t *testing.T) {
	t.Setenv("SOURCEWATCH_CONFIG", "")
	t.Setenv("SOURCEWATCH_DISCORD_TOKEN", "tok")
	t.Setenv("SOURCEWATCH_SERVERS", "a:27015,b:27016")
	c, err := Load()
	if err != nil || c.DiscordToken != "tok" || len(c.Servers) != 2 {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestLoadFileMode(t *testing.T) {
	needFileModes(t)
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"webhook_url":"u","servers":["a:1","b:2"]}`), 0o644)
	t.Setenv("SOURCEWATCH_CONFIG", p)
	if _, err := Load(); err == nil {
		t.Fatal("want error for mode 644")
	}
	os.Chmod(p, 0o600)
	c, err := Load()
	if err != nil || c.WebhookURL != "u" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestServerListIsTrimmed(t *testing.T) {
	t.Setenv("SOURCEWATCH_CONFIG", "")
	t.Setenv("SOURCEWATCH_DISCORD_TOKEN", "")
	t.Setenv("SOURCEWATCH_SERVERS", " a:27015 , b:27016,,")
	c, err := Load()
	if err != nil || !reflect.DeepEqual(c.Servers, []string{"a:27015", "b:27016"}) {
		t.Fatalf("got %q, %v", c.Servers, err)
	}
	// Only commas is no server at all.
	t.Setenv("SOURCEWATCH_SERVERS", ",")
	if _, err := Load(); err == nil {
		t.Fatal("want the error for no servers and no token")
	}
}

func TestServerListInTheFileIsTrimmed(t *testing.T) {
	needFileModes(t)
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"servers":[" a:27015", "", "b:27016 "]}`), 0o600)
	t.Setenv("SOURCEWATCH_CONFIG", p)
	t.Setenv("SOURCEWATCH_SERVERS", "")
	c, err := Load()
	if err != nil || !reflect.DeepEqual(c.Servers, []string{"a:27015", "b:27016"}) {
		t.Fatalf("got %q, %v", c.Servers, err)
	}
}

func TestLogRedacts(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "config", Config{DiscordToken: "SECRET", WebhookURL: "SECRETURL"})
	if strings.Contains(buf.String(), "SECRET") {
		t.Fatal(buf.String())
	}
}

// --- the project was renamed: the old names still work ---

func TestOldEnvironmentNamesStillWork(t *testing.T) {
	for _, name := range []string{"CONFIG", "DISCORD_TOKEN", "WEBHOOK_URL", "SERVERS", "DATABASE"} {
		t.Setenv("SOURCEWATCH_"+name, "")
		t.Setenv("PAWNZ_"+name, "")
	}
	t.Setenv("PAWNZ_DISCORD_TOKEN", "old-token")
	t.Setenv("PAWNZ_SERVERS", "a:1,b:2,c:3")
	t.Setenv("PAWNZ_DATABASE", "/data/old.db")
	c, err := Load()
	if err != nil || c.DiscordToken != "old-token" || len(c.Servers) != 3 || c.Database != "/data/old.db" {
		t.Fatalf("the old names must still be read: %+v, %v", c, err)
	}
}

func TestNewEnvironmentNamesWinOverOldOnes(t *testing.T) {
	t.Setenv("PAWNZ_DISCORD_TOKEN", "old")
	t.Setenv("SOURCEWATCH_DISCORD_TOKEN", "new")
	t.Setenv("PAWNZ_CONFIG", "")
	t.Setenv("SOURCEWATCH_CONFIG", "")
	t.Setenv("PAWNZ_SERVERS", "")
	t.Setenv("SOURCEWATCH_SERVERS", "")
	if c, err := Load(); err != nil || c.DiscordToken != "new" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestOldConfigVariableStillPointsAtTheFile(t *testing.T) {
	needFileModes(t)
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"discord_token":"from-file"}`), 0o600)
	t.Setenv("SOURCEWATCH_CONFIG", "")
	t.Setenv("PAWNZ_CONFIG", p)
	t.Setenv("PAWNZ_DISCORD_TOKEN", "")
	t.Setenv("SOURCEWATCH_DISCORD_TOKEN", "")
	if c, err := Load(); err != nil || c.DiscordToken != "from-file" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestDatabaseNameKeepsAnExistingOldFile(t *testing.T) {
	t.Setenv("SOURCEWATCH_DATABASE", "")
	t.Setenv("PAWNZ_DATABASE", "")
	t.Setenv("SOURCEWATCH_DISCORD_TOKEN", "t")
	t.Setenv("SOURCEWATCH_CONFIG", "")
	t.Setenv("PAWNZ_CONFIG", "")

	t.Chdir(t.TempDir())
	if c, _ := Load(); c.Database != "sourcewatch.db" {
		t.Errorf("a new install uses %q", c.Database)
	}

	os.WriteFile("pawnz.db", []byte("x"), 0o600)
	if c, _ := Load(); c.Database != "pawnz.db" {
		t.Errorf("an install that has only the old file keeps it, not an empty new database: %q", c.Database)
	}

	os.WriteFile("sourcewatch.db", []byte("x"), 0o600)
	if c, _ := Load(); c.Database != "sourcewatch.db" {
		t.Errorf("once the new file exists it wins: %q", c.Database)
	}

	t.Setenv("SOURCEWATCH_DATABASE", "/elsewhere.db")
	if c, _ := Load(); c.Database != "/elsewhere.db" {
		t.Errorf("a configured path always wins: %q", c.Database)
	}
}

// TestEverySettingIsInTheREADME keeps the configuration reference of the README complete: every
// field of the file and every SOURCEWATCH_ variable that Load reads must be named there.
func TestEverySettingIsInTheREADME(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		names = append(names, "`"+ct.Field(i).Tag.Get("json")+"`")
	}
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`env\("([A-Z_]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		names = append(names, "`SOURCEWATCH_"+m[1]+"`")
	}
	if len(names) < ct.NumField()+5 {
		t.Fatalf("found only %v: the test no longer sees how Load reads its settings", names)
	}
	for _, n := range names {
		if !strings.Contains(string(readme), n) {
			t.Errorf("the README does not mention %s", n)
		}
	}
}
