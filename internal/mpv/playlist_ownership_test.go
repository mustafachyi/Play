package mpv

import "testing"

func TestPlaylistHooksDoNotModifyForeignPlayback(t *testing.T) {
	content := `local host = require "mp"
local function run()
    local hooks, writes = {}, {}
    local path = ""
    local fake = {
        add_key_binding = function() end,
        register_event = function() end,
        add_hook = function(name, priority, callback)
            hooks[name] = hooks[name] or {}
            hooks[name][#hooks[name] + 1] = callback
        end,
        get_property = function(name, fallback)
            if name == "path" or name == "stream-open-filename" then return path end
            if name == "playlist/2/title" then return "Playlist title" end
            if name == "metadata/by-key/title" then return "Metadata title" end
            return fallback
        end,
        get_property_number = function(name, fallback)
            if name == "playlist-playing-pos" then return 2 end
            return fallback
        end,
        set_property = function(name, value)
            writes[#writes + 1] = {name, value}
            return true
        end,
    }
    package.loaded["mp"] = fake
` + playlistScript(2, 90, true, "", "", "", "http://127.0.0.1:1234/token/item/") + `
    for _, foreign in ipairs({
        "foreign-file.mkv",
        "http://127.0.0.1:1234/foreign/item/0003.edl",
        "http://127.0.0.1:1234/token/itemish/0003.edl",
        "http://example.com/item/0003.edl",
        "",
    }) do
        path = foreign
        hooks.on_load[1]()
        hooks.on_preloaded[1]()
        assert(#writes == 0, "owned playlist hooks modified foreign playback: " .. foreign)
    end
    path = "http://127.0.0.1:1234/token/item/0003.edl"
    hooks.on_load[1]()
    assert(#writes == 3, "owned item did not receive title, artwork and pending start")
    assert(writes[1][1] == "file-local-options/force-media-title" and writes[1][2] == "Playlist title")
    assert(writes[2][1] == "file-local-options/cover-art-files" and writes[2][2] == "http://127.0.0.1:1234/token/cover/0003")
    assert(writes[3][1] == "file-local-options/start" and writes[3][2] == "90", "foreign load consumed the pending start")
    hooks.on_preloaded[1]()
    assert(#writes == 4 and writes[4][2] == "Metadata title")
    hooks.on_load[1]()
    assert(#writes == 6, "initial seek was applied more than once")
end
local success, failure = pcall(run)
package.loaded["mp"] = host
if not success then host.msg.error(tostring(failure)) end
host.commandv("quit", success and 0 or 1)
`
	runScriptCheck(t, content)
}
