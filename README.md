# discstream

Download the AppImage from the [latest release](https://github.com/Slycat555/discstream/releases/latest), make it executable (`chmod +x`), and run it.

Streams the disc in your optical drive to every device on your local network.

- **Blu-ray, DVD-Video and audio CD** are detected automatically.
- **Encrypted discs** (DVD CSS, Blu-ray AACS/BD+) are decrypted with **MakeMKV**, which writes into a pipe that ffmpeg reads. The disc is never ripped to disk.
- **Unencrypted discs** are read directly by ffmpeg (libbluray/libdvdnav).
- **Output** is HLS (H.264 + AAC stereo). It plays in any browser, VLC, mpv, Kodi, Infuse, Apple TV and similar players.
- **Packaging:** a single portable AppImage. It bundles a static ffmpeg and needs nothing from your distro.

## Usage

```sh
./discstream-1.0.1.AppImage                 # stream /dev/sr0 on port 8080
```

It prints the addresses to open, for example:

```
streaming on:
  browser:        http://192.168.1.48:8080/
  VLC/mpv/Kodi:   http://192.168.1.48:8080/live/index.m3u8
```

If no disc is in the drive, it waits for one. Playback starts with the longest title.

### Tray icon

discstream puts an icon in the system tray. Its menu shows what is playing and how much is cached, and has these items:
- **Open player** opens the web player in your browser. Left-clicking the icon does the same.
- **Show log** opens the log file, `~/.cache/discstream/discstream.log`.
- **Quit** stops streaming and deletes the cache.

You can also start the AppImage from your file manager or app menu, without a terminal.
The icon needs a tray that supports StatusNotifier, as KDE Plasma, XFCE and Cinnamon do; on GNOME install the *AppIndicator* extension.
Without a tray, discstream runs in the terminal as before and Ctrl+C quits. Use `-tray=false` to turn the icon off.

### Caching and seeking

The disc is read and encoded as fast as your machine allows, ahead of playback. The cached part is kept on disk, so you can seek
back and forward anywhere within it. The page shows a progress bar, e.g. "Cached 12:30 of 47:21".
Fully cached titles are marked **Cached**; switching back to one plays it instantly without reading the disc again.
A title that was only partly cached when you switched away is discarded, because the disc is read from the start of the title.

- **Speed:** with re-encoding, caching ran at about 10× real time for a 1080p Blu-ray on a fast desktop. It's much faster with `-copy-video` or for DVDs.
- **Disk space:** the cache needs about 1–3 GB per hour of 1080p video, in `~/.cache/discstream`. It is deleted when you quit.
- **`-realtime`:** reads only at playback speed, as earlier versions did. It uses less CPU and disk, but you can't seek forward.

### Choosing an episode

The web page lists every title on the disc with its length. Click one to switch the stream to it.
The page reloads the player on its own, and playback starts after roughly 10–15 seconds while the drive seeks.

- **Blu-ray:** entries are playlists. **DVD:** titles. **Encrypted discs:** MakeMKV titles. **Audio CD:** tracks; the stream starts at the chosen track and continues to the end of the disc.
- A title made of the other titles played back to back is labelled **Play all**; this is common on TV series discs.
- When a title ends, pick another one or press Ctrl+C.
- The switch applies to everyone watching. VLC and other players keep the same URL, but they need to reopen it after a switch.

Common options (`--help` lists them all):

| option | default | meaning |
|---|---|---|
| `-device` | `/dev/sr0` | drive, or an ISO file / VIDEO_TS / BDMV folder |
| `-port` | `8080` | HTTP port |
| `-title N` | longest | title to start with (the IDs listed at start-up) |
| `-audio N` | `0` | audio track index; the track list is printed at start-up |
| `-makemkv` | `auto` | `auto` = only for encrypted discs, `always`, `never` |
| `-crf`, `-preset`, `-maxrate`, `-max-height` | `21`, `veryfast`, `8M`, `1080` | x264 quality and speed |
| `-copy-video` | off | pass H.264 video through without re-encoding (less CPU) |
| `-realtime` | off | read at playback speed instead of caching ahead |
| `-window N` | `0` | keep only the last N segments (implies `-realtime`) |
| `-tray` | `true` | show the system tray icon |
| `-exit` | off | quit when the first title ends |


## Requirements

- **x86_64 Linux** and read access to the drive (usually the `cdrom` or `optical` group).
- **MakeMKV, for encrypted discs.** Its licence doesn't allow bundling it, so install it separately:
  - native: https://www.makemkv.com/forum/viewtopic.php?f=3&t=224 (`makemkvcon` on `PATH`), or
  - Flatpak: `flatpak install flathub com.makemkv.MakeMKV` (detected automatically), or
  - any other location: `MAKEMKVCON=/path/to/makemkvcon ./discstream-1.0.1.AppImage`

  MakeMKV needs a valid beta or registration key. Its messages are shown in the log.
- **FUSE**, as for any AppImage. Without FUSE, run it with `APPIMAGE_EXTRACT_AND_RUN=1`.

## How audio stays in sync

Discs often start audio a fraction of a second after video. Blu-ray playlists and DVD cells also produce timestamp jumps at clip boundaries. ffmpeg handles this in three ways:

- `-fflags +genpts` gives every packet a timestamp.
- Video is forced to a constant frame rate, so frames are duplicated or dropped to follow the timestamps.
- Audio goes through `aresample=async=1`, which fills gaps in the middle with silence instead of sliding audio against video. A late audio start is kept as it is on the disc, so it lines up with the picture.

Measured against the disc itself, by matching scene cuts and the audio waveform, the stream's audio is within about 20 ms of the video. That remaining 20 ms is the AAC encoder's start-up delay, which is too small to notice.


## How encryption is detected

- **Blu-ray:** libbluray reports AACS/BD+ on the disc. The host's libaacs is disabled for this check, so the result reflects the disc.
- **DVD:** sectors are sampled across the disc to check the MPEG `PES_scrambling_control` bits that CSS sets. Unreadable sectors on an unauthenticated drive are also treated as CSS.
- If the result is unclear, MakeMKV is used.

## Building

```sh
./build-appimage.sh      # writes dist/discstream-<version>.AppImage; downloads Go, a static ffmpeg
                         # and appimagetool into .build-cache/ on first run
```

## Limitations

- Subtitles are not included in the stream.
- UHD Blu-ray HDR is converted to 8-bit without tone-mapping, so colours look washed out.
- 1080p re-encoding needs a CPU that can run x264 `veryfast` in real time; use `-preset ultrafast` or `-copy-video` on slow machines.
