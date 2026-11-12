package mpv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func RunPlaylist(ctx context.Context, request PlaylistRequest) (runErr error) {
	if err := validatePlaylistRequest(request); err != nil {
		return err
	}
	var err error
	helperPath := ""
	pageDirectory := ""
	if request.PageURL != "" {
		helperPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate playlist helper: %w", err)
		}
		pageDirectory, err = os.MkdirTemp("", "play-pages-")
		if err != nil {
			return fmt.Errorf("create playlist page directory: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(pageDirectory); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("remove playlist pages: %w", err))
			}
		}()
	}
	scriptPath, err := writePlaylistScript(request.StartIndex, request.StartSeconds, request.AudioOnly, request.PageURL, helperPath, pageDirectory, strings.TrimSuffix(request.URL, "/playlist.m3u")+"/item/")
	if err != nil {
		return fmt.Errorf("create playlist integration script: %w", err)
	}
	runErr = runCommand(exec.CommandContext(ctx, request.Executable, playlistArguments(request, scriptPath)...), request.Started)
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
	args := append(windowArguments(),
		"--terminal=no",
		"--ytdl=no",
		"--vid=auto",
		"--aid=1",
		"--sid=auto",
		"--sub-visibility=no",
		"--watch-later-options-remove=sub-pos",
		"--script-opts-append=playview-preview=playlist",
		"--script="+scriptPath,
		"--playlist-start="+strconv.Itoa(request.StartIndex),
	)
	if request.AudioOnly {
		args = append(args, "--force-window=yes", "--audio-display=external-first")
	}
	return append(args, "--playlist="+request.URL)
}

func writePlaylistScript(startIndex int, startSeconds int64, audioOnly bool, pageURL, helperPath, pageDirectory, sourcePrefix string) (string, error) {
	return writeScript(playlistScript(startIndex, startSeconds, audioOnly, pageURL, helperPath, pageDirectory, sourcePrefix))
}

func luaString(value string) string {
	var result strings.Builder
	result.WriteByte('"')
	for _, value := range []byte(value) {
		switch value {
		case '"', '\\':
			result.WriteByte('\\')
			result.WriteByte(value)
		default:
			if value < 32 || value >= 127 {
				fmt.Fprintf(&result, "\\%03d", value)
			} else {
				result.WriteByte(value)
			}
		}
	}
	result.WriteByte('"')
	return result.String()
}

func playlistScript(startIndex int, startSeconds int64, audioOnly bool, pageURL, helperPath, pageDirectory, sourcePrefix string) string {
	return fmt.Sprintf(`local mp = require "mp"
local start_index = %d
local start_seconds = %d
local audio_only = %t
local page_url = %s
local helper_path = %s
local page_directory = %s
local source_prefix = %s
local start_applied = false
local pagination_started = false
local next_page = 2
local pagination_stopped = false
local active_request
local retry_timer
local page_attempts = 0
local expected_count = 0
local owned_prefix = page_url:gsub("/page/$", "/item/")

local function owns_file()
    local path = mp.get_property("path", "")
    return source_prefix ~= "" and path:sub(1, #source_prefix) == source_prefix
end

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

local stop_pagination
local function check_owner()
    if not pagination_started or pagination_stopped then return end
    local first = mp.get_property("playlist/0/filename", "")
    if first:sub(1, #owned_prefix) ~= owned_prefix or mp.get_property_number("playlist-count", 0) ~= expected_count then
        stop_pagination()
    end
end

stop_pagination = function()
    if pagination_stopped then return end
    pagination_stopped = true
    mp.unobserve_property(check_owner)
    local timer = retry_timer
    retry_timer = nil
    if timer then timer:kill() end
    local request = active_request
    active_request = nil
    if request then mp.abort_async_command(request.handle) end
end

local load_next_page
load_next_page = function()
    if page_url == "" or pagination_stopped or active_request or retry_timer then
        return
    end
    check_owner()
    if pagination_stopped then return end
    local filename = string.format("%%04d.m3u", next_page)
    local request = {}
    active_request = request
    page_attempts = page_attempts + 1
    request.handle = mp.command_native_async({
        name = "subprocess", args = {helper_path, "-playlist-page", page_url .. filename, page_directory},
        capture_stderr = true, capture_size = 4096, playback_only = false,
    }, function(success, result)
        if active_request ~= request or pagination_stopped then return end
        active_request = nil
        check_owner()
        if pagination_stopped then return end
        if type(result) == "table" and result.status == 75 then
            mp.msg.error("Could not load more videos: the server requested a longer wait. Reopen the playlist later.")
            stop_pagination()
            return
        end
        if not success or type(result) ~= "table" or result.status ~= 0 then
            if page_attempts < 3 then
                local timer
                timer = mp.add_timeout(page_attempts == 1 and 0.15 or 0.4, function()
                    if retry_timer ~= timer or pagination_stopped then return end
                    retry_timer = nil
                    load_next_page()
                end)
                retry_timer = timer
            else
                mp.msg.error("Could not load more videos; will try again when another video starts")
            end
            return
        end
        local _, command_error = mp.command_native({"loadlist", page_directory .. "/" .. filename, "append"})
        if command_error then stop_pagination() return end
        local after = mp.get_property_number("playlist-count", 0)
        if after <= expected_count then
            stop_pagination()
            return
        end
        expected_count = after
        next_page = next_page + 1
        page_attempts = 0
        load_next_page()
    end)
end

local function start_pagination()
    if page_url == "" or pagination_stopped then
        return
    end
    if pagination_started then
        if page_attempts >= 3 and not active_request and not retry_timer then
            page_attempts = 0
            load_next_page()
        end
        return
    end
    pagination_started = true
    expected_count = mp.get_property_number("playlist-count", 0)
    mp.observe_property("playlist/0/filename", "string", check_owner)
    mp.observe_property("playlist-count", "number", check_owner)
    load_next_page()
end

mp.add_key_binding("_", "play-cycle-video", cycle_video)
mp.register_event("file-loaded", start_pagination)
mp.register_event("shutdown", stop_pagination)

mp.add_hook("on_load", 50, function()
    if not owns_file() then return end
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
    if not owns_file() then return end
    local title = mp.get_property("metadata/by-key/title", "")
    if title ~= "" then
        mp.set_property("file-local-options/force-media-title", title)
    end
end)
`, startIndex, startSeconds, audioOnly, luaString(pageURL), luaString(helperPath), luaString(pageDirectory), luaString(sourcePrefix)) + subtitleScript(sourcePrefix, false) + chapterScript(sourcePrefix, false)
}
