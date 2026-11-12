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

You can also pass a playlist URL or video ID. Run `play` without arguments to enter one at the prompt.

Play returns to the prompt after playback starts. Close the mpv window to stop playback.

The player opens centered at a size that fits your screen. You can resize it or switch to full screen.

| Option | Description |
| --- | --- |
| `-a` | Play audio only |
| `-wait` | Wait for playback to end before returning to the prompt |
| `-h`, `-help` | Show help |
| `-version` | Show the version |

YouTube Mix playlists and live streams are not supported.

## PlayView (optional)

For custom controls and hover previews, extract `PlayView.zip` into your mpv configuration folder:

| Installation | Folder |
| --- | --- |
| Scoop (default) | `%USERPROFILE%\scoop\apps\mpv\current\portable_config` |
| mpv (default configuration) | `%APPDATA%\mpv` |

For a custom Scoop location, run `scoop prefix mpv` and use its `portable_config` folder. For other portable installations, use mpv's active configuration folder.

Add this to `mpv.conf` in that folder:

```text
osc=no
```

## Development

Use Go 1.27.3 or later. From the project root:

```powershell
go build -o play.exe .
go vet ./...
go test -count=1 ./...
```

Playback integration tests require `mpv.exe` on `PATH`. They are skipped if it is unavailable.

## License

[GNU General Public License v3.0 or later](LICENSE).
