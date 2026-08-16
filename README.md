# Play

Play YouTube videos and playlists with [mpv](https://mpv.io/) on Windows.

Play accepts YouTube URLs, playlist URLs, and video IDs. It supports video playback, audio only playback, multiple media tracks, subtitles, timestamps, and playlists.

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

Run Play without an argument to enter a YouTube URL, playlist URL, or video ID interactively:

```cmd
play
```

`-a` plays audio only.

`-h` and `-help` show help.

`-version` shows the installed version.

YouTube Mix playlists and live streams are not supported.

## ModernZ

For a more complete player experience, releases include an optional `ModernZ.zip`.

The included package is based on [ModernZ](https://github.com/Samillion/ModernZ) and has been modified to add a video track control. This allows the video quality provided by Play to be changed directly from the player while a video is running.

ModernZ is optional. Play works normally without it.

### Install with Scoop

If mpv was installed through Scoop, extract the contents of `ModernZ.zip` into the mpv `portable_config` directory.

For a standard per user Scoop installation, the directory is:

```text
%USERPROFILE%\scoop\apps\mpv\current\portable_config
```

If Scoop is installed somewhere else, find the mpv installation directory with:

```cmd
scoop prefix mpv
```

Open the returned directory and extract `ModernZ.zip` into its `portable_config` folder.

### Install with mpv

For a regular Windows mpv installation, extract the contents of `ModernZ.zip` into:

```text
%APPDATA%\mpv
```

If the directory does not exist, create it.

If your mpv installation already has a `portable_config` directory beside `mpv.exe`, use that directory instead.

The archive already contains the required structure:

```text
fonts
    modernz-icons.ttf

script-opts
    modernz.conf

scripts
    modernz.lua
```

No additional folders need to be created.

### Configure mpv

Open `mpv.conf` in the same mpv configuration directory used above.

If `mpv.conf` does not exist, create a plain text file named exactly `mpv.conf`.

Add:

```text
osc=no
border=no # Optional, but recommended
```

`osc=no` disables the standard mpv interface so ModernZ can provide the player controls.

`border=no` removes the native window border and is optional.

The included ModernZ package is a modified version intended for use with Play. ModernZ remains subject to its own license.

## Requirements

Windows x64

Internet connection

mpv

## License

Play is licensed under the GNU General Public License version 3 or later. See `LICENSE` for details.