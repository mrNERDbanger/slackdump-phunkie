package slackdump

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/trace"
	"sync"

	"golang.org/x/time/rate"

	"github.com/pkg/errors"
	"github.com/slack-go/slack"
)

// Files structure is used for downloading conversation files.
type Files struct {
	Files     []slack.File
	ChannelID string
}

// ChannelFiles returns files from the conversation.
func (sd *SlackDumper) ChannelFiles(ch *Channel) *Files {
	return &Files{
		Files:     sd.filesFromMessages(ch.Messages),
		ChannelID: ch.ID,
	}
}

// filesFromMessages extracts files from messages slice.
func (sd *SlackDumper) filesFromMessages(m []Message) []slack.File {
	var files []slack.File

	for i := range m {
		if m[i].Files != nil {
			files = append(files, m[i].Files...)
		}
		// include threaded files
		for _, reply := range m[i].ThreadReplies {
			files = append(files, reply.Files...)
		}
	}
	return files
}

// SaveFileTo saves file to the specified directory.
func (sd *SlackDumper) SaveFileTo(ctx context.Context, l *rate.Limiter, dir string, f *slack.File) (int64, error) {
	filePath := filepath.Join(dir, filename(f))
	file, err := os.Create(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if err := sd.client.GetFile(f.URLPrivateDownload, file); err != nil {
		return 0, errors.WithStack(err)
	}

	trace.WithRegion(ctx, "limiter.file", func() {
		l.Wait(ctx)
	})

	return int64(f.Size), nil
}

// filename returns name of the file
func filename(f *slack.File) string {
	return fmt.Sprintf("%s-%s", f.ID, f.Name)
}

// fileDownloader will downloadstarts an sd.numDownloaders goroutines to
// download files in parallel.  It will download any files that were received on toDownload channel,
// and will close "done" once all downloads are complete.
func (sd *SlackDumper) fileDownloader(ctx context.Context, l *rate.Limiter, dir string, toDownload <-chan *slack.File) (chan struct{}, error) {
	done := make(chan struct{})

	if !sd.options.dumpfiles {
		// terminating if dumpfiles is not enabled.
		close(done)
		return done, nil
	}

	if err := os.Mkdir(dir, 0777); err != nil {
		if !os.IsExist(err) {
			// channels done is closed by defer
			return done, err
		}
	}

	var wg sync.WaitGroup
	go func() {
		// create workers
		for i := 0; i < sd.options.workers; i++ {
			wg.Add(1)
			go func() {
				sd.worker(ctx, l, dir, seenFilter(toDownload))
				wg.Done()
			}()
		}
	}()

	// sentinel
	go func() {
		wg.Wait()
		close(done)
	}()

	return done, nil
}

func (sd *SlackDumper) worker(ctx context.Context, l *rate.Limiter, dir string, filesC <-chan *slack.File) {
	for file := range filesC {
		// download file
		log.Printf("saving %s, size: %d", filename(file), file.Size)
		n, err := sd.SaveFileTo(ctx, l, dir, file)
		if err != nil {
			log.Printf("error saving %q: %s", filename(file), err)
		}
		log.Printf("file %s saved: %d bytes written", filename(file), n)
	}
}

// seenFilter filters the files from filesC to ensure that no duplicates
// are downloaded.
func seenFilter(filesC <-chan *slack.File) <-chan *slack.File {
	dlQ := make(chan *slack.File)
	go func() {
		// closing stop will lead to all worker goroutines to terminate.
		defer close(dlQ)

		// seen contains file ids that already been seen,
		// so we don't download the same file twice
		seen := make(map[string]bool)
		// files queue must be closed by the caller (see DumpToDir.(1))
		for f := range filesC {
			if _, ok := seen[f.ID]; ok {
				log.Printf("already seen %s, skipping", filename(f))
				continue
			}
			seen[f.ID] = true
			dlQ <- f
		}
	}()
	return dlQ
}
