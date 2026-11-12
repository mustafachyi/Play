package mpv

import "testing"

func playlistRecoveryCheck(t *testing.T, assertions string) {
	t.Helper()
	content := `local host = require "mp"
local function run()
    local events, observers, requests, timers = {}, {}, {}, {}
    local first = "http://127.0.0.1:1234/token/item/0001.edl"
    local count, appended, growth = 2, 0, 1
    local appendError
    local messages = {}
    local fake = {
        add_key_binding = function() end,
        add_hook = function() end,
        register_event = function(name, callback) events[name] = callback end,
        observe_property = function(name, kind, callback) observers[name] = callback end,
        unobserve_property = function(callback)
            for name, current in pairs(observers) do
                if current == callback then observers[name] = nil end
            end
        end,
        get_property = function(name, fallback)
            if name == "playlist/0/filename" then return first end
            return fallback
        end,
        get_property_number = function(name, fallback)
            if name == "playlist-count" then return count end
            return fallback
        end,
        command_native_async = function(command, callback)
            requests[#requests + 1] = {command = command, callback = callback}
            return #requests
        end,
        abort_async_command = function(handle) requests[handle].aborted = true end,
        command_native = function(command)
            assert(command[1] == "loadlist" and command[3] == "append")
            if appendError then return nil, appendError end
            count, appended = count + growth, appended + 1
            return true
        end,
        add_timeout = function(seconds, callback)
            local timer = {active = true, seconds = seconds, callback = callback}
            timer.kill = function(self) self.active = false end
            timers[#timers + 1] = timer
            return timer
        end,
        msg = {
            error = function(message) messages[#messages + 1] = message end,
            warn = function(message) messages[#messages + 1] = message end,
        },
    }
    package.loaded["mp"] = fake
    local function runTimer()
        for _, timer in ipairs(timers) do
            if timer.active then
                timer.active = false
                timer.callback()
                return true
            end
        end
        return false
    end
    local function assertPage(index, page)
        assert(requests[index] and requests[index].command.args[3]:match(string.format("/%04d.m3u$", page)), "pagination requested an unexpected page")
    end
` + playlistScript(0, 0, false, "http://127.0.0.1:1234/token/page/", "play.exe", "pages", "http://127.0.0.1:1234/token/item/") + assertions + `
end
local success, failure = pcall(run)
package.loaded["mp"] = host
if not success then host.msg.error(tostring(failure)) end
host.commandv("quit", success and 0 or 1)
`
	runScriptCheck(t, content)
}

func TestPlaylistPaginationRetriesTransientHelperFailure(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    assertPage(1, 2)
    requests[1].callback(false, {status = 1, stderr = "Temporary continuation failure"})
    assert(#requests == 1 and appended == 0, "failed page advanced pagination")
    assert(runTimer(), "transient page failure did not schedule a retry")
    assertPage(2, 2)
    requests[1].callback(true, {status = 0})
    assert(appended == 0 and #requests == 2, "obsolete helper completion modified the active page")
    requests[2].callback(true, {status = 0})
    assert(appended == 1 and count == 3, "recovered page was not appended exactly once")
    assertPage(3, 3)
    events["shutdown"]()
    assert(requests[3].aborted, "shutdown did not abort the active page helper")
    requests[3].callback(true, {status = 0})
    assert(appended == 1 and not runTimer(), "late callback restarted pagination after shutdown")
`)
}

func TestPlaylistPaginationIgnoresReplacedRetryTimer(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    requests[1].callback(false, {status = 1})
    local oldTimer = timers[1]
    assert(runTimer(), "first retry was not scheduled")
    requests[2].callback(false, {status = 1})
    local currentTimer = timers[2]
    assert(currentTimer and currentTimer.active, "replacement retry was not scheduled")
    oldTimer.callback()
    assert(#requests == 2 and currentTimer.active, "obsolete timer modified the active retry")
    assert(runTimer(), "replacement retry did not run")
    assertPage(3, 2)
    events["shutdown"]()
`)
}

func TestPlaylistPaginationBoundsFailuresAndResumesOnLaterFile(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    for attempt = 1, 3 do
        assertPage(attempt, 2)
        requests[attempt].callback(false, {status = 1})
        if attempt < 3 then assert(runTimer(), "page retry was not scheduled") end
    end
    assert(#requests == 3 and appended == 0 and not runTimer(), "failed page retries did not stop after three attempts without appending")
    assert(next(observers) ~= nil, "playlist ownership monitoring stopped after retry exhaustion")
    assert(#messages == 1, "retry exhaustion did not produce exactly one diagnostic")
    events["file-loaded"]()
    assertPage(4, 2)
    requests[4].callback(true, {status = 0})
    assert(appended == 1, "later file load did not recover the pending page")
    assertPage(5, 3)
    requests[5].callback(false, {status = 1})
    assert(runTimer(), "successful page did not reset the next page retry budget")
    assertPage(6, 3)
    events["shutdown"]()
`)
}

func TestPlaylistPaginationCancelsScheduledRetriesOnOwnerChange(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    requests[1].callback(false, {status = 1})
    assert(timers[1] and timers[1].active, "pending retry was not scheduled")
    local staleTimer = timers[1]
    first = "http://127.0.0.1:1234/foreign/item/0001.edl"
    observers["playlist/0/filename"]()
    assert(not staleTimer.active and next(observers) == nil, "playlist owner change did not cancel the pending retry and remove its observers")
    staleTimer.callback()
    events["file-loaded"]()
    assert(#requests == 1 and appended == 0 and not runTimer(), "late retry restarted an unrelated playlist")
`)
}

func TestPlaylistPaginationCancelsScheduledRetriesOnShutdown(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    requests[1].callback(false, {status = 1})
    local staleTimer = timers[1]
    assert(staleTimer and staleTimer.active, "pending retry was not scheduled")
    events["shutdown"]()
    assert(not staleTimer.active and next(observers) == nil, "shutdown did not cancel the pending retry and remove its observers")
    staleTimer.callback()
    requests[1].callback(true, {status = 0})
    assert(#requests == 1 and appended == 0 and not runTimer(), "late callback restarted pagination after shutdown")
`)
}

func TestPlaylistPaginationDoesNotRetryUncertainAppendFailures(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    appendError = "Append failed"
    requests[1].callback(true, {status = 0})
    events["file-loaded"]()
    assert(#requests == 1 and appended == 0 and not runTimer() and next(observers) == nil, "uncertain append result was retried")
`)
}

func TestPlaylistPaginationTerminalPageDoesNotResume(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    growth = 0
    requests[1].callback(true, {status = 0})
    events["file-loaded"]()
    assert(#requests == 1 and appended == 1 and not runTimer() and next(observers) == nil, "empty terminal page restarted pagination")
`)
}

func TestPlaylistPaginationHonorsHelperCooldown(t *testing.T) {
	playlistRecoveryCheck(t, `
    events["file-loaded"]()
    requests[1].callback(true, {status = 75})
    events["file-loaded"]()
    requests[1].callback(true, {status = 0})
    assert(#requests == 1 and appended == 0 and not runTimer() and next(observers) == nil, "pagination retried a page after an upstream cooldown")
    assert(#messages == 1, "terminal cooldown did not produce exactly one diagnostic")
`)
}
