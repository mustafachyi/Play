package mpv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLuaStringPreservesUnicodeAndEscapes(t *testing.T) {
	for _, test := range []struct{ value, want string }{
		{"C:\\folder\\play-\u2028.exe", `"C:\\folder\\play-\226\128\168.exe"`},
		{"a\"b\\c\n\x009", `"a\"b\\c\010\0009"`},
	} {
		if actual := luaString(test.value); actual != test.want {
			t.Fatalf("Lua string = %q; want %q", actual, test.want)
		}
	}
}

func TestLuaStringNativeByteRoundTrip(t *testing.T) {
	bytes := make([]byte, 256)
	for index := range bytes {
		bytes[index] = byte(index)
	}
	samples := []string{
		"",
		string(bytes),
		"\x009\x7f1\xff7",
		"C:\\quoted \"path\"\\雪\u2028.exe",
		strings.Repeat("\x00\"\\\n\xff9", 1024),
	}
	var source strings.Builder
	source.WriteString("local function run()\n")
	for _, value := range samples {
		fmt.Fprintf(&source, "do local value = %s\nassert(#value == %d)\nlocal expected = {", luaString(value), len(value))
		for _, item := range []byte(value) {
			fmt.Fprintf(&source, "%d,", item)
		}
		source.WriteString("}\nfor index, byte in ipairs(expected) do assert(value:byte(index) == byte, 'Lua string byte round trip failed') end\nend\n")
	}
	source.WriteString("end\nlocal success, failure = pcall(run)\nif not success then mp.msg.error(tostring(failure)) end\nmp.commandv('quit', success and 0 or 1)\n")
	runScriptCheck(t, source.String())
}

func TestPlaylistCancellationCleansPageDirectory(t *testing.T) {
	t.Setenv("PLAY_TEST_MPV", "wait")
	directory := t.TempDir()
	t.Setenv("TMP", directory)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := PlaylistRequest{
		Executable: executable,
		URL:        "http://127.0.0.1:1234/token/playlist.m3u",
		PageURL:    "http://127.0.0.1:1234/token/page/",
		Started:    func() error { cancel(); return nil },
	}
	if err := RunPlaylist(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("playlist cancellation error = %v; want context.Canceled", err)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files = %v, error = %v; want no files and no error", files, err)
	}
}
