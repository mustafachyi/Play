package mpv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

func RunPlaylist(ctx context.Context, request PlaylistRequest) error {
	if err := validatePlaylistRequest(request); err != nil {
		return err
	}
	scriptPath, err := writePlaylistScript(request.StartIndex, request.StartSeconds, request.AudioOnly, request.PageURL)
	if err != nil {
		return fmt.Errorf("create playlist integration script: %w", err)
	}
	runErr := exec.CommandContext(ctx, request.Executable, playlistArguments(request, scriptPath)...).Run()
	return errors.Join(normalizeRunError(ctx, runErr), removeTemp(scriptPath, "playlist integration script"))
}

func validatePlaylistRequest(request PlaylistRequest) error {
	if request.Executable == "" {
		return errors.New("mpv executable path is empty")
	}
	if !localHTTPURL(request.URL) {
		return errors.New("mpv playlist URL must use the local playlist server")
	}
	if request.PageURL != "" && !localHTTPURL(request.PageURL) {
		return errors.New("mpv playlist page URL must use the local playlist server")
	}
	if request.StartIndex < 0 || request.StartSeconds < 0 {
		return errors.New("playlist start position is invalid")
	}
	return nil
}

func playlistArguments(request PlaylistRequest, scriptPath string) []string {
	args := []string{
		"--terminal=no",
		"--ytdl=no",
		"--vid=auto",
		"--aid=1",
		"--sid=auto",
		"--sub-visibility=no",
		"--watch-later-options-remove=sub-pos",
		"--script=" + scriptPath,
		"--playlist-start=" + strconv.Itoa(request.StartIndex),
	}
	if request.AudioOnly {
		args = append(args, "--force-window=yes", "--audio-display=external-first")
	}
	return append(args, "--playlist="+request.URL)
}

func writePlaylistScript(startIndex int, startSeconds int64, audioOnly bool, pageURL string) (string, error) {
	file, err := os.CreateTemp("", "play-*.lua")
	if err != nil {
		return "", err
	}
	name := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(name)
	}
	if _, err := file.WriteString(playlistScript(startIndex, startSeconds, audioOnly, pageURL)); err != nil {
		cleanup()
		return "", err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", err
	}
	return name, nil
}

func playlistScript(startIndex int, startSeconds int64, audioOnly bool, pageURL string) string {
	return fmt.Sprintf(`local mp = require "mp"
local start_index = %d
local start_seconds = %d
local audio_only = %t
local page_url = %q
local start_applied = false
local pagination_started = false
local next_page = 2

local function cycle_video()
    local tracks = mp.get_property_native("track-list") or {}
    local ids = {}
    local current = nil
    for _, track in ipairs(tracks) do
        if track.type == "video" and not track.albumart then
            ids[#ids + 1] = track.id
            if track.selected then
                current = #ids
            end
        end
    end
    if #ids == 0 then
        return
    end
    local next_index = 1
    if current ~= nil then
        next_index = current %% #ids + 1
    end
    mp.set_property_number("vid", ids[next_index])
end

local function cover_url(filename)
    local cover, count = filename:gsub("/item/(%%d+)%%.edl$", "/cover/%%1")
    if count == 1 then
        return cover
    end
    return ""
end

local load_next_page
load_next_page = function()
    if page_url == "" then
        return
    end
    local before = mp.get_property_number("playlist-count", 0)
    local url = string.format("%%s%%04d.m3u", page_url, next_page)
    mp.command_native_async({"loadlist", url, "append"}, function(success)
        if not success then
            return
        end
        local after = mp.get_property_number("playlist-count", 0)
        if after <= before then
            return
        end
        next_page = next_page + 1
        load_next_page()
    end)
end

local function start_pagination()
    if pagination_started or page_url == "" then
        return
    end
    pagination_started = true
    load_next_page()
end

mp.add_key_binding("_", "play-cycle-video", cycle_video)
mp.register_event("file-loaded", start_pagination)

mp.add_hook("on_load", 50, function()
    local playlist_pos = mp.get_property_number("playlist-playing-pos", -1)
    if playlist_pos >= 0 then
        local playlist_title = mp.get_property("playlist/" .. tostring(playlist_pos) .. "/title", "")
        if playlist_title ~= "" then
            mp.set_property("file-local-options/force-media-title", playlist_title)
        end
    end
    if audio_only then
        local cover = cover_url(mp.get_property("stream-open-filename", ""))
        if cover ~= "" then
            mp.set_property("file-local-options/cover-art-files", cover)
        end
    end
    if not start_applied and start_seconds > 0 and playlist_pos == start_index then
        start_applied = true
        mp.set_property("file-local-options/start", tostring(start_seconds))
    end
end)

mp.add_hook("on_preloaded", 50, function()
    local title = mp.get_property("metadata/by-key/title", "")
    if title ~= "" then
        mp.set_property("file-local-options/force-media-title", title)
    end
end)
`, startIndex, startSeconds, audioOnly, pageURL)
}
