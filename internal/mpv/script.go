package mpv

import (
	"errors"
	"fmt"
	"os"
)

func writeScript(content string) (string, error) {
	file, err := os.CreateTemp("", "play-*.lua")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err := file.WriteString(content); err != nil {
		return "", errors.Join(err, file.Close(), removeTemp(name, "integration script"))
	}
	if err := file.Close(); err != nil {
		return "", errors.Join(err, removeTemp(name, "integration script"))
	}
	return name, nil
}

func subtitleScript(source string, exact bool) string {
	return fmt.Sprintf(`do
    local utils = require "mp.utils"
    local owner = %s
    local exact = %t
    mp.add_hook("on_preloaded", 50, function(hook)
        local path = mp.get_property("path", "")
        if owner == "" or (exact and path ~= owner) or (not exact and path:sub(1, #owner) ~= owner) then return end
        local source = mp.get_property("metadata/by-key/play-subtitles", "")
        if source == "" then return end
        local subtitles = utils.parse_json(source)
        if type(subtitles) ~= "table" then
            mp.msg.error("Could not load subtitles: invalid subtitle metadata")
            return
        end
        hook:defer()
        local index = 0
        local function load_next()
            index = index + 1
            local subtitle = subtitles[index]
            if subtitle == nil then hook:cont(); return end
            if type(subtitle) ~= "table" or type(subtitle.source) ~= "string" or type(subtitle.title) ~= "string" or type(subtitle.language) ~= "string" then
                mp.msg.error("Could not load subtitles: invalid subtitle track metadata")
                hook:cont()
                return
            end
            mp.command_native_async({"sub-add", subtitle.source, "auto", subtitle.title, subtitle.language}, function(success, result, error)
                if not success then mp.msg.error("Could not load subtitles: " .. tostring(error or result)) end
                load_next()
            end)
        end
        load_next()
    end)
end
`, luaString(source), exact)
}

func chapterScript(source string, exact bool) string {
	return fmt.Sprintf(`do
    local utils = require "mp.utils"
    local owner = %s
    local exact = %t
    mp.add_hook("on_preloaded", 50, function()
        local path = mp.get_property("path", "")
        if owner == "" or (exact and path ~= owner) or (not exact and path:sub(1, #owner) ~= owner) then return end
        local source = mp.get_property("metadata/by-key/play-chapters", "")
        local chapters = source ~= "" and utils.parse_json(source) or {}
        if type(chapters) ~= "table" then
            mp.msg.error("Could not load chapters: invalid chapter metadata")
            chapters = {}
        end
        local success, error = mp.set_property_native("chapter-list", chapters)
        if not success then mp.msg.error("Could not load chapters: " .. tostring(error)) end
    end)
end
`, luaString(source), exact)
}
