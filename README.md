# Play

Play YouTube videos and playlists with [mpv](https://mpv.io/) on Windows x64.

## Install

Install with Scoop:

```cmd
scoop bucket add play https://github.com/mustafachyi/Play
scoop install play/play
```

Scoop also installs mpv. To update Play:

```cmd
scoop update
scoop update play
```

For a manual installation, download `play.exe` from the [latest release](https://github.com/mustafachyi/Play/releases/latest), install mpv, and add both executable folders to `PATH`.

## Usage

```cmd
play "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
```

Pass a video URL, playlist URL, or video ID. Run `play` without arguments for an input prompt.

Play returns to the prompt after playback starts. Close the mpv window to stop playback.

The player opens centered and fits your screen. Resize the window or switch to full screen.

| Option | Description |
| --- | --- |
| `-a` | Play audio only |
| `-wait` | Wait for playback to end before returning to the prompt |
| `-h`, `-help` | Show help |
| `-version` | Show the version |
| `-browser` | Manage Firefox integration |

YouTube Mix playlists and live streams are not supported.

## Firefox integration (optional)

Use the **YouTube Play** extension to open videos and playlists in Play from the latest Firefox release.

1. Download the signed **YouTube Play** `.xpi` from the [latest release](https://github.com/mustafachyi/Play/releases/latest), open it in Firefox, and confirm installation.
2. Run `play -browser`, enable integration, and select your `mpv.exe`.
3. Open the extension's **Preferences**, select **Check connection**, and refresh YouTube.

The setup menu also includes **Status**, **Repair**, and **Disable**. Run `play -browser help` for direct commands.

After moving a portable copy, run `.\play.exe -browser enable --switch` from its new folder. Disable integration before deleting the copy.

## PlayView (optional)

For custom controls and hover previews, extract `PlayView.zip` into your mpv configuration folder:

| Installation | Folder |
| --- | --- |
| Scoop (default) | `%USERPROFILE%\scoop\apps\mpv\current\portable_config` |
| mpv (default configuration) | `%APPDATA%\mpv` |

For a custom Scoop location, run `scoop prefix mpv` and use its `portable_config` folder. For other portable installations, use mpv's active configuration folder.

PlayView activates in mpv windows opened by Play. Other mpv sessions keep their existing controls and settings.

Set `mode=on` in `script-opts/playview.conf` to use PlayView in every mpv session, or `mode=off` to disable it. The default is `mode=auto`. Changes apply to new mpv windows.

## Development

Use Go 1.27.4 or later. From the project root:

```powershell
go build -o play.exe .
gofmt -l .
go vet ./...
go test -count=1 ./...
```

Playback integration tests require `mpv.exe` on `PATH`. They are skipped if it is unavailable.

## License

[GNU General Public License v3.0 or later](LICENSE).
