package claude

import (
	"path/filepath"
	"testing"
)

// Just before a harvest saves, every nickname is taken from the file as it is then: a rename or a
// clear made while the harvest ran wins over the copy it read at the start, and a nickname the
// harvest never saw is kept.
func TestKeepNicknamesTakesTheFilesNicknames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	if err := SaveMetadata(path, map[string]SessionMetadata{
		"s1": {Title: "on disk", Nickname: "B"},
		"s3": {Title: "only on disk", Nickname: "C"},
	}); err != nil {
		t.Fatal(err)
	}
	meta := map[string]SessionMetadata{
		"s1": {Title: "harvested", Nickname: "A"},
		"s2": {Title: "harvested", Nickname: "cleared since"},
	}
	keepNicknames(path, meta)
	if got := meta["s1"]; got.Nickname != "B" || got.Title != "harvested" {
		t.Errorf("s1 = %+v, want the file's nickname with the harvest's title", got)
	}
	if got := meta["s2"].Nickname; got != "" {
		t.Errorf("s2 nickname = %q, want it cleared as the file has it", got)
	}
	if got := meta["s3"]; got.Nickname != "C" || got.Title != "" {
		t.Errorf("s3 = %+v, want only the nickname carried", got)
	}

	// A file deleted while the harvest ran took its nicknames with it.
	gone := map[string]SessionMetadata{"s1": {Title: "t", Nickname: "A"}}
	keepNicknames(filepath.Join(t.TempDir(), "missing.json"), gone)
	if got := gone["s1"]; got.Nickname != "" || got.Title != "t" {
		t.Errorf("after the file was deleted: %+v, want the title without the nickname", got)
	}
}
