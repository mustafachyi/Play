# Play

Play YouTube videos and playlists with [mpv](https://mpv.io/) on Windows.

Play accepts YouTube URLs, playlist URLs, and video IDs. It supports normal video playback, audio-only playback, multiple available media tracks, subtitles, timestamps, and playlists.

## Installation

### Scoop

Add the bucket:

```cmd
scoop bucket add play https://github.com/mustafachyi/Play
```

Install Play:

```cmd
scoop install play/play
```

Scoop installs mpv automatically as a dependency.

To update Play:

```cmd
scoop update play
```

### Manual

Download `play.exe` from the latest GitHub release and place it somewhere on your `PATH`.

mpv must also be installed and available as `mpv.exe` on `PATH`.

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

Run without an argument to enter a YouTube URL or video ID interactively:

```cmd
play
```

Available options:

- `-a` — play audio only
- `-h`, `-help` — show help
- `-version` — show the installed version

YouTube Mix playlists and live streams are not supported.

## Requirements

- Windows x64
- Internet connection
- mpv

## License

Licensed under the GNU General Public License v3.0 or later. See `LICENSE` for details.