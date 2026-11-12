package mpv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteScriptPreservesContentAndClosesFile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMP", directory)
	content := "local value = \"quoted, content\"\n"
	path, err := writeScript(content)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != directory || !strings.HasPrefix(filepath.Base(path), "play-") || !strings.HasSuffix(path, ".lua") {
		t.Fatalf("unexpected script path %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("script content = %q, error = %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove script: %v", err)
	}
}

func TestWriteScriptReportsCreationFailure(t *testing.T) {
	t.Setenv("TMP", filepath.Join(t.TempDir(), "missing", "directory"))
	path, err := writeScript(subtitleScript("owned.edl", true))
	if err == nil || path != "" {
		t.Fatalf("creation failure path = %q, error = %v", path, err)
	}
}

func TestSubtitleScriptDefersAndContinuesNativeHook(t *testing.T) {
	content := `local utils = require "mp.utils"
local host = mp
local function run()
    local registered, source, pending, command
    local path = "owned.edl"
    local errors = {}
    mp = {
        add_hook = function(name, priority, callback)
            assert(name == "on_preloaded" and priority == 50)
            assert(registered == nil)
            registered = callback
        end,
        get_property = function(name, fallback)
            if name == "path" then return path end
            assert(name == "metadata/by-key/play-subtitles" and fallback == "")
            return source or fallback
        end,
        command_native_async = function(value, callback)
            assert(pending == nil)
            command, pending = value, callback
        end,
        msg = {error = function(message) errors[#errors + 1] = message end},
    }
` + subtitleScript("owned.edl", true) + `
    local function hook()
        return {
            deferred = 0, continued = 0,
            defer = function(self) self.deferred = self.deferred + 1 end,
            cont = function(self) self.continued = self.continued + 1 end,
        }
    end
    assert(type(registered) == "function")
    for _, foreign in ipairs({"", "foreign.edl", "owned.edl.foreign"}) do
        path, source = foreign, utils.format_json({{source = "edl://first", title = "English", language = "en"}})
        local entry = hook()
        registered(entry)
        assert(entry.deferred == 0 and entry.continued == 0 and pending == nil)
    end
    path = "owned.edl"
    for _, missing in ipairs({false, ""}) do
        source = missing
        local entry = hook()
        registered(entry)
        assert(entry.deferred == 0 and entry.continued == 0 and pending == nil)
    end
    for _, result in ipairs({{true}, {false, "HTTP failure"}, {false, "operation aborted"}}) do
        source = utils.format_json({{source = "edl://first", title = "English", language = "en"}, {source = "edl://second", title = "French", language = "fr"}})
        local entry = hook()
        registered(entry)
        assert(entry.deferred == 1 and entry.continued == 0)
        assert(#command == 5 and command[1] == "sub-add" and command[2] == "edl://first" and command[3] == "auto" and command[4] == "English" and command[5] == "en")
        local callback = pending
        pending = nil
        local before = #errors
        callback(result[1], nil, result[2])
        assert(entry.deferred == 1 and entry.continued == 0)
        assert(pending ~= nil and command[2] == "edl://second" and command[4] == "French" and command[5] == "fr")
        callback = pending
        pending = nil
        callback(true, nil, nil)
        assert(entry.continued == 1)
        if result[1] then
            assert(#errors == before)
        else
            assert(#errors == before + 1 and errors[#errors]:find(result[2], 1, true))
        end
    end
    for _, invalid in ipairs({
        {source = "not JSON", deferred = 0},
        {source = "42", deferred = 0},
        {source = "null", deferred = 0},
        {source = "[{}]", deferred = 1},
        {source = "[false]", deferred = 1},
        {source = '[{"source":42,"title":"English","language":"en"}]', deferred = 1},
    }) do
        source = invalid.source
        local entry = hook()
        local before = #errors
        registered(entry)
        assert(entry.deferred == invalid.deferred and entry.continued == invalid.deferred and pending == nil)
        assert(#errors == before + 1 and errors[#errors]:find("invalid subtitle", 1, true))
    end
end
local success, failure = pcall(run)
mp = host
if not success then host.msg.error(tostring(failure)) end
host.commandv("quit", success and 0 or 1)
`
	runScriptCheck(t, content)
}

func TestSubtitleScriptChecksPrefixAndEmptyOwners(t *testing.T) {
	for _, sample := range []struct {
		name  string
		owner string
	}{
		{"prefix", "http://127.0.0.1:1234/token/item/"},
		{"empty", ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			content := `local utils = require "mp.utils"
local host = mp
local function run()
    local owner = ` + luaString(sample.owner) + `
    local path, registered, pending
    local metadataReads = 0
    mp = {
        add_hook = function(name, priority, callback)
            assert(name == "on_preloaded" and priority == 50 and registered == nil)
            registered = callback
        end,
        get_property = function(name, fallback)
            assert(fallback == "")
            if name == "path" then return path end
            assert(name == "metadata/by-key/play-subtitles")
            metadataReads = metadataReads + 1
            return utils.format_json({{source = "edl://first", title = "English", language = "en"}})
        end,
        command_native_async = function(command, callback)
            assert(command[1] == "sub-add" and command[3] == "auto" and pending == nil)
            pending = callback
        end,
        msg = {error = function(message) error(message) end},
    }
` + subtitleScript(sample.owner, false) + `
    for _, file in ipairs({owner .. "01.edl", owner .. "02.edl", "foreign" .. owner .. "03.edl", "http://127.0.0.1:1234/foreign/04.edl", ""}) do
        path = file
        local entry = {
            deferred = 0, continued = 0,
            defer = function(self) self.deferred = self.deferred + 1 end,
            cont = function(self) self.continued = self.continued + 1 end,
        }
        local before = metadataReads
        registered(entry)
        local owned = owner ~= "" and file:sub(1, #owner) == owner
        if owned then
            assert(metadataReads == before + 1 and entry.deferred == 1 and entry.continued == 0)
            local callback = pending
            pending = nil
            callback(true, nil, nil)
            assert(entry.continued == 1)
        else
            assert(metadataReads == before and entry.deferred == 0 and entry.continued == 0 and pending == nil)
        end
    end
end
local success, failure = pcall(run)
mp = host
if not success then host.msg.error(tostring(failure)) end
host.commandv("quit", success and 0 or 1)
`
			runScriptCheck(t, content)
		})
	}
}

func runScriptCheck(t *testing.T, content string) {
	t.Helper()
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	path := filepath.Join(t.TempDir(), "subtitle-hook.lua")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--no-config", "--load-scripts=no", "--idle=yes", "--vo=null", "--ao=null", "--input-terminal=no", "--media-controls=no", "--script="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native Lua fixture error = %v\n%s", err, output)
	}
}
