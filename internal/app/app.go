package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"play/internal/media"
	"play/internal/mpv"
	"play/internal/stream"
	"play/internal/youtube"
)

const maxConsoleInput = 4096

type options struct {
	audioOnly bool
	input     string
	help      bool
	version   bool
}

type playlistSource interface {
	Next(context.Context) ([]media.PlaylistItem, bool, error)
}

type dependencies struct {
	resolveVideo        func(context.Context, string) (media.Item, error)
	openPlaylist        func(string) (playlistSource, error)
	findMPV             func() (string, error)
	startGateway        func([]stream.Resource, stream.Reporter) (gateway, error)
	startDynamicGateway func(stream.Reporter) (gateway, error)
	play                func(context.Context, mpv.Request) error
	playPlaylist        func(context.Context, mpv.PlaylistRequest) error
	closeSource         func()
}

type gateway struct {
	url   func(string) string
	add   func([]stream.Resource) error
	close func() error
}

type console struct {
	scanner *bufio.Scanner
	out     io.Writer
}

func Run(ctx context.Context, version string, args []string, in io.Reader, out, errOut io.Writer) error {
	deps := defaultDependencies()
	if deps.closeSource != nil {
		defer deps.closeSource()
	}
	return run(ctx, version, args, in, out, errOut, deps)
}

func run(ctx context.Context, version string, args []string, in io.Reader, out, errOut io.Writer, deps dependencies) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	if opts.help {
		printUsage(out)
		return nil
	}
	if opts.version {
		fmt.Fprintf(out, "play %s\n", version)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if errOut == nil {
		errOut = io.Discard
	}

	var reference youtube.Reference
	if opts.input == "" {
		reference, err = promptReference(ctx, newConsole(in, out))
		if err != nil {
			return err
		}
	} else {
		var ok bool
		reference, ok = youtube.Parse(opts.input)
		if !ok {
			return errors.New("invalid YouTube URL, playlist URL, or video ID")
		}
	}
	if youtube.IsMixPlaylistID(reference.PlaylistID) {
		return errors.New("YouTube Mix playlists are not supported")
	}

	mpvPath, err := deps.findMPV()
	if err != nil {
		return err
	}
	if reference.PlaylistID != "" {
		return runPlaylist(ctx, reference, opts.audioOnly, mpvPath, errOut, deps)
	}
	return runVideo(ctx, reference, opts.audioOnly, mpvPath, errOut, deps)
}

func resolveVideo(ctx context.Context, videoID string, deps dependencies) (media.Item, error) {
	item, err := deps.resolveVideo(ctx, videoID)
	if err == nil {
		return item, nil
	}
	if errors.Is(err, youtube.ErrLiveUnsupported) {
		return media.Item{}, err
	}
	return media.Item{}, fmt.Errorf("resolve video: %w", err)
}

func defaultDependencies() dependencies {
	source := youtube.NewClient()
	return dependencies{
		resolveVideo: source.Video,
		openPlaylist: func(playlistID string) (playlistSource, error) {
			return source.Playlist(playlistID)
		},
		findMPV: mpv.Find,
		startGateway: func(resources []stream.Resource, reporter stream.Reporter) (gateway, error) {
			server, err := stream.Start(resources, reporter)
			if err != nil {
				return gateway{}, err
			}
			return gatewayFromServer(server), nil
		},
		startDynamicGateway: func(reporter stream.Reporter) (gateway, error) {
			server, err := stream.StartDynamic(reporter)
			if err != nil {
				return gateway{}, err
			}
			return gatewayFromServer(server), nil
		},
		play:         mpv.Run,
		playPlaylist: mpv.RunPlaylist,
		closeSource:  source.Close,
	}
}

func gatewayFromServer(server *stream.Server) gateway {
	return gateway{url: server.URL, add: server.Add, close: server.Close}
}

func gatewayCloseError(local gateway) error {
	if local.close == nil {
		return nil
	}
	if err := local.close(); err != nil {
		return fmt.Errorf("stop local stream server: %w", err)
	}
	return nil
}

func parseArgs(args []string) (options, error) {
	var opts options
	for _, arg := range args {
		switch arg {
		case "-a":
			if opts.audioOnly {
				return options{}, errors.New("-a may be specified only once")
			}
			opts.audioOnly = true
		case "-h", "-help":
			if opts.help {
				return options{}, errors.New("help option may be specified only once")
			}
			opts.help = true
		case "-version":
			if opts.version {
				return options{}, errors.New("-version may be specified only once")
			}
			opts.version = true
		default:
			if strings.HasPrefix(arg, "-") {
				if _, ok := youtube.Parse(arg); !ok {
					return options{}, fmt.Errorf("unknown option %q", arg)
				}
			}
			if opts.input != "" {
				return options{}, errors.New("only one YouTube URL, playlist URL, or video ID may be provided")
			}
			opts.input = arg
		}
	}
	if opts.help && (opts.version || opts.audioOnly || opts.input != "") {
		return options{}, errors.New("-help cannot be combined with playback options")
	}
	if opts.version && (opts.help || opts.audioOnly || opts.input != "") {
		return options{}, errors.New("-version cannot be combined with playback options")
	}
	return opts, nil
}

func newReporter(out io.Writer) stream.Reporter {
	var mu sync.Mutex
	return func(name string, err error) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(out, "play: stream %s: %v\n", name, err)
	}
}

func newConsole(in io.Reader, out io.Writer) *console {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024), maxConsoleInput)
	return &console{scanner: scanner, out: out}
}

func (c *console) readLine(ctx context.Context, prompt string) (string, error) {
	if _, err := fmt.Fprint(c.out, prompt); err != nil {
		return "", err
	}

	type result struct {
		line string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		if c.scanner.Scan() {
			resultCh <- result{line: strings.TrimSpace(c.scanner.Text())}
			return
		}
		if err := c.scanner.Err(); err != nil {
			resultCh <- result{err: fmt.Errorf("read input: %w", err)}
			return
		}
		resultCh <- result{err: io.EOF}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-resultCh:
		return result.line, result.err
	}
}

func promptReference(ctx context.Context, terminal *console) (youtube.Reference, error) {
	for {
		value, err := terminal.readLine(ctx, "YouTube URL, playlist URL, or video ID: ")
		if err != nil {
			return youtube.Reference{}, err
		}
		if reference, ok := youtube.Parse(value); ok {
			return reference, nil
		}
		if _, err := fmt.Fprintln(terminal.out, "Invalid YouTube URL, playlist URL, or video ID."); err != nil {
			return youtube.Reference{}, err
		}
	}
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  play [youtube-url|playlist-url|video-id]")
	fmt.Fprintln(out, "  play -a [youtube-url|playlist-url|video-id]")
	fmt.Fprintln(out, "  play -h")
	fmt.Fprintln(out, "  play -version")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -a        Play audio only")
	fmt.Fprintln(out, "  -h, -help Show help")
	fmt.Fprintln(out, "  -version  Show version")
}
