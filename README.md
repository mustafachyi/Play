# Play

Play YouTube videos and playlists with [mpv](https://mpv.io/) on Windows. Supports audio-only playback, multiple tracks, subtitles, and timestamps.

## Installation

### Scoop

```cmd
scoop bucket add play https://github.com/mustafachyi/Play
scoop install play/play
```

Scoop installs mpv automatically. To update:

```cmd
scoop update
scoop update play
```

### Manual

Download `play.exe` from the [latest release](https://github.com/mustafachyi/Play/releases/latest) and add it to your `PATH`. Install mpv and ensure `mpv.exe` is also on your `PATH`.

## Usage

Play a video:

```cmd
play "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
```

Play audio only:

```cmd
play -a "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
```

Play a playlist:

```cmd
play "https://www.youtube.com/playlist?list=PLAYLIST_ID"
```

Video IDs are also accepted. Run `play` without an argument to enter a URL or ID interactively.

Playback runs in the background, leaving the terminal available. Close mpv to stop playback. Use `-wait` to keep the command attached until playback ends.

| Option | Description |
| --- | --- |
| `-a` | Play audio only |
| `-wait` | Wait for playback to end |
| `-h`, `-help` | Show help |
| `-version` | Show version |

YouTube Mix playlists and live streams are not supported.

## ModernZ

The optional `ModernZ.zip` contains a modified version of [ModernZ](https://github.com/Samillion/ModernZ) with a video quality control for Play.

Extract the archive into your mpv configuration directory:

| Installation | Directory |
| --- | --- |
| Scoop | `%USERPROFILE%\scoop\apps\mpv\current\portable_config` |
| Regular mpv | `%APPDATA%\mpv` |

For a custom Scoop location, run `scoop prefix mpv` and use the `portable_config` directory there. If mpv already has a `portable_config` directory beside `mpv.exe`, use it.

Create or open `mpv.conf` in that directory and add:

```text
osc=no
```

Optionally, add `border=no` to remove the window border.

ModernZ retains its own license.

## Requirements

- Windows x64
- Internet connection
- mpv

Building from source requires Go 1.27.3 or later.

## License

[GNU General Public License v3.0 or later](LICENSE).
