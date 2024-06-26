package client

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/nats-io/nats.go"
	"github.com/simpleiot/simpleiot/data"
	"github.com/simpleiot/simpleiot/system"
)

// Update represents the config of a metrics node type
type Update struct {
	ID              string   `node:"id"`
	Parent          string   `node:"parent"`
	Description     string   `point:"description"`
	VersionOS       string   `point:"versionOS"`
	URI             string   `point:"uri"`
	OSUpdates       []string `point:"osUpdate"`
	DownloadOS      string   `point:"downloadOS"`
	OSDownloaded    string   `point:"osDownloaded"`
	DiscardDownload string   `point:"discardDownload"`
	Prefix          string   `point:"prefix"`
	Directory       string   `point:"directory"`
	PollPeriod      int      `point:"pollPeriod"`
	Refresh         bool     `point:"refresh"`
	AutoDownload    bool     `point:"autoDownload"`
	AutoReboot      bool     `point:"autoReboot"`
}

// UpdateClient is a SIOT client used to collect system or app metrics
type UpdateClient struct {
	log           *log.Logger
	nc            *nats.Conn
	config        Update
	stop          chan struct{}
	newPoints     chan NewPoints
	newEdgePoints chan NewPoints
}

// NewUpdateClient ...
func NewUpdateClient(nc *nats.Conn, config Update) Client {
	return &UpdateClient{
		log:           log.New(os.Stderr, "Update: ", log.LstdFlags|log.Lmsgprefix),
		nc:            nc,
		config:        config,
		stop:          make(chan struct{}),
		newPoints:     make(chan NewPoints),
		newEdgePoints: make(chan NewPoints),
	}
}

func (m *UpdateClient) setError(err error) {
	errS := ""
	if err != nil {
		errS = err.Error()
		m.log.Println(err)
	}

	p := data.Point{
		Type: data.PointTypeError,
		Time: time.Now(),
		Text: errS,
	}

	e := SendNodePoint(m.nc, m.config.ID, p, true)
	if e != nil {
		m.log.Println("error sending point:", e)
	}
}

var reUpd = regexp.MustCompile(`(.*)_(\d+\.\d+\.\d+)\.upd`)

// Run the main logic for this client and blocks until stopped
func (m *UpdateClient) Run() error {
	cDownloadFinished := make(chan struct{})
	// cSetError is used in any goroutines
	cSetError := make(chan error)

	download := func(v string) error {
		defer func() {
			cDownloadFinished <- struct{}{}
			_ = SendNodePoint(m.nc, m.config.ID,
				data.Point{Time: time.Now(), Type: data.PointTypeDownloadOS, Text: ""},
				false,
			)
			m.config.DownloadOS = ""
		}()

		u, err := url.JoinPath(m.config.URI, m.config.Prefix+"_"+v+".upd")
		if err != nil {
			return fmt.Errorf("URI error: %w", err)
		}

		m.log.Println("Downloading update: ", u)

		fileName := filepath.Base(u)
		destPath := filepath.Join(m.config.Directory, fileName)

		out, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("Error creating OS update file: %w", err)
		}
		defer out.Close()

		resp, err := http.Get(u)
		if err != nil {
			return fmt.Errorf("Error http get fetching OS update: %w", err)
		}
		defer resp.Body.Close()

		c, err := io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("io.Copy error: %w", err)
		}

		if c <= 0 {
			os.Remove(destPath)
			return fmt.Errorf("Failed to download: %v", u)
		}

		return nil
	}

	// fill in default prefix
	if m.config.Prefix == "" {
		p, err := os.Hostname()
		if err != nil {
			m.log.Println("Error getting hostname: ", err)
		} else {
			m.log.Println("Setting update prefix to: ", p)
			err := SendNodePoint(m.nc, m.config.ID, data.Point{
				Time: time.Now(),
				Type: data.PointTypePrefix,
				Key:  "0",
				Text: p}, false)
			if err != nil {
				m.log.Println("Error sending point: ", err)
			} else {
				m.config.Prefix = p
			}
		}
	}

	if m.config.Directory == "" {
		d := "/data"
		m.log.Println("Setting directory to: ", d)
		err := SendNodePoint(m.nc, m.config.ID, data.Point{
			Time: time.Now(),
			Type: data.PointTypeDirectory,
			Key:  "0",
			Text: d}, false)
		if err != nil {
			m.log.Println("Error sending point: ", err)
		} else {
			m.config.Directory = d
		}
	}

	if m.config.PollPeriod <= 0 {
		p := 30
		m.log.Println("Setting poll period to: ", p)
		err := SendNodePoint(m.nc, m.config.ID, data.Point{
			Time:  time.Now(),
			Type:  data.PointTypePollPeriod,
			Key:   "0",
			Value: float64(p)}, false)
		if err != nil {
			m.log.Println("Error sending point: ", err)
		} else {
			m.config.PollPeriod = p
		}
	}

	getUpdates := func() error {
		clearUpdateList := func() {
			cnt := len(m.config.OSUpdates)

			if cnt > 0 {
				pts := data.Points{}
				for i := 0; i < cnt; i++ {
					pts = append(pts, data.Point{
						Time: time.Now(), Type: data.PointTypeOSUpdate, Key: strconv.Itoa(i), Tombstone: 1,
					})
				}

				err := SendNodePoints(m.nc, m.config.ID, pts, false)
				if err != nil {
					m.log.Println("Error sending version points: ", err)
				}
			}
		}

		p, err := url.JoinPath(m.config.URI, "files.txt")
		if err != nil {
			clearUpdateList()
			return fmt.Errorf("URI error: %w", err)
		}
		resp, err := http.Get(p)
		if err != nil {
			clearUpdateList()
			return fmt.Errorf("Error getting updates: %w", err)
		}

		if resp.StatusCode != 200 {
			clearUpdateList()
			return fmt.Errorf("Error getting updates: %v", resp.Status)
		}

		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("Error reading http response: %w", err)
		}

		updates := strings.Split(string(body), "\n")

		updates = slices.DeleteFunc(updates, func(u string) bool {
			return !strings.HasPrefix(u, m.config.Prefix)
		})

		versions := semver.Versions{}

		for _, u := range updates {
			matches := reUpd.FindStringSubmatch(u)
			if len(matches) > 1 {
				prefix := matches[1]
				version := matches[2]
				sv, err := semver.Parse(version)
				if err != nil {
					m.log.Printf("Error parsing version %v: %v\n", version, err)
				}
				if prefix == m.config.Prefix {
					versions = append(versions, sv)
				}
			} else {
				m.log.Println("Version not found in filename: ", u)
			}
		}

		sort.Sort(versions)

		// need to update versions available
		pts := data.Points{}
		now := time.Now()
		for i, v := range versions {
			pts = append(pts, data.Point{
				Time: now, Type: data.PointTypeOSUpdate, Text: v.String(), Key: strconv.Itoa(i),
			})
		}

		err = SendNodePoints(m.nc, m.config.ID, pts, false)
		if err != nil {
			m.log.Println("Error sending version points: ", err)

		}

		err = data.MergePoints(m.config.ID, pts, &m.config)
		if err != nil {
			log.Println("error merging new points:", err)
		}

		underflowCount := len(m.config.OSUpdates) - len(versions)

		if underflowCount > 0 {
			pts := data.Points{}
			for i := len(versions); i < len(versions)+underflowCount; i++ {
				pts = append(pts, data.Point{
					Time: now, Type: data.PointTypeOSUpdate, Key: strconv.Itoa(i), Tombstone: 1,
				})
			}

			err = SendNodePoints(m.nc, m.config.ID, pts, false)
			if err != nil {
				m.log.Println("Error sending version points: ", err)
			}
		}
		return nil
	}

	cleanDownloads := func() error {
		files, err := os.ReadDir(m.config.Directory)
		var errRet error
		if err != nil {
			return fmt.Errorf("Error getting files in data dir: %w", err)
		}

		for _, file := range files {
			if !file.IsDir() && filepath.Ext(file.Name()) == ".upd" {
				p := filepath.Join(m.config.Directory, file.Name())
				err = os.Remove(p)
				if err != nil {
					m.log.Printf("Error removing %v: %v\n", file.Name(), err)
					errRet = err
				}
			}
		}

		m.config.OSDownloaded = ""
		err = SendNodePoint(m.nc, m.config.ID, data.Point{
			Time: time.Now(),
			Type: data.PointTypeOSDownloaded,
			Text: "",
			Key:  "0",
		}, true)
		if err != nil {
			m.log.Println("Error clearing downloaded point: ", err)
		}

		err = SendNodePoints(m.nc, m.config.ID, data.Points{
			{Time: time.Now(), Type: data.PointTypeDiscardDownload, Value: 0},
		}, true)
		if err != nil {
			m.log.Println("Error discarding download: ", err)
		}

		return errRet
	}

	checkDownloads := func() error {
		files, err := os.ReadDir(m.config.Directory)
		if err != nil {
			return fmt.Errorf("Error getting files in data dir: %w", err)
		}

		updFiles := []string{}
		for _, file := range files {
			if !file.IsDir() && filepath.Ext(file.Name()) == ".upd" {
				updFiles = append(updFiles, file.Name())
			}
		}

		versions := semver.Versions{}
		for _, u := range updFiles {

			matches := reUpd.FindStringSubmatch(u)
			if len(matches) > 1 {
				prefix := matches[1]
				version := matches[2]
				sv, err := semver.Parse(version)
				if err != nil {
					m.log.Printf("Error parsing version %v: %v\n", version, err)
				}
				if prefix == m.config.Prefix {
					versions = append(versions, sv)
				}
			} else {
				m.log.Println("Version not found in filename: ", u)
			}
		}

		sort.Sort(versions)

		if len(versions) > 0 {
			m.config.OSDownloaded = versions[len(versions)-1].String()
			err := SendNodePoint(m.nc, m.config.ID, data.Point{
				Time: time.Now(),
				Type: data.PointTypeOSDownloaded,
				Key:  "0",
				Text: m.config.OSDownloaded}, true)

			if err != nil {
				m.log.Println("Error sending point: ", err)
			}
		} else {
			m.config.OSDownloaded = ""
			err = SendNodePoint(m.nc, m.config.ID, data.Point{
				Time: time.Now(),
				Type: data.PointTypeOSDownloaded,
				Text: "",
				Key:  "0",
			}, true)
			if err != nil {
				m.log.Println("Error clearing downloaded point: ", err)
			}
		}
		return nil
	}

	reboot := func() {
		err := exec.Command("reboot").Run()
		if err != nil {
			m.log.Println("Error rebooting: ", err)
		} else {
			m.log.Println("Rebooting ...")
		}
	}

	autoDownload := func() error {
		newestUpdate := ""
		if len(m.config.OSUpdates) > 0 {
			newestUpdate = m.config.OSUpdates[len(m.config.OSUpdates)-1]
		} else {
			return nil
		}

		currentOSV, err := semver.Parse(m.config.VersionOS)
		if err != nil {
			return fmt.Errorf("Autodownload, Error parsing current OS version: %w", err)
		}

		newestUpdateV, err := semver.Parse(newestUpdate)
		if err != nil {
			return fmt.Errorf("autodownload: Error parsing newest OS update version: %w", err)
		}

		if newestUpdateV.GT(currentOSV) &&
			newestUpdate != m.config.OSDownloaded &&
			newestUpdate != m.config.DownloadOS {
			// download a newer update
			err := SendNodePoint(m.nc, m.config.ID, data.Point{
				Time: time.Now(),
				Type: data.PointTypeDownloadOS,
				Text: newestUpdate,
			}, true)
			if err != nil {
				return fmt.Errorf("Error sending point: %w", err)
			}
			m.config.DownloadOS = newestUpdate

			go func(f string) {
				err := download(f)
				if err != nil {
					cSetError <- fmt.Errorf("error downloading update: %w", err)
				}
			}(newestUpdate)
		}
		return nil
	}

	m.setError(nil)
	err := getUpdates()
	if err != nil {
		m.setError(err)
	}
	err = checkDownloads()
	if err != nil {
		m.setError(err)
	}

	osVersion, err := system.ReadOSVersion("VERSION_ID")
	if err != nil {
		m.log.Println("Error reading OS version: ", err)
	} else {
		err := SendNodePoint(m.nc, m.config.ID, data.Point{
			Time: time.Now(),
			Type: data.PointTypeVersionOS,
			Key:  "0",
			Text: osVersion.String(),
		}, true)

		if err != nil {
			m.log.Println("Error sending OS version point: ", err)
		}

		m.config.VersionOS = osVersion.String()
	}

	if m.config.DownloadOS != "" {
		go func() {
			err := download(m.config.DownloadOS)
			if err != nil {
				cSetError <- fmt.Errorf("Error downloading file: %w", err)
			}
		}()
	}

	checkTickerTime := time.Minute * time.Duration(m.config.PollPeriod)
	checkTicker := time.NewTicker(checkTickerTime)
	if m.config.AutoDownload {
		m.setError(nil)
		err := getUpdates()
		if err != nil {
			m.setError(err)
		} else {
			err := autoDownload()
			if err != nil {
				m.setError(err)
			}
		}
	}

done:
	for {
		select {
		case <-m.stop:
			break done

		case pts := <-m.newPoints:
			err := data.MergePoints(pts.ID, pts.Points, &m.config)
			if err != nil {
				log.Println("error merging new points:", err)
			}

			for _, p := range pts.Points {
				switch p.Type {
				case data.PointTypeDownloadOS:
					if p.Text != "" {
						go func(f string) {
							err := download(f)
							if err != nil {
								cSetError <- fmt.Errorf("Error downloading update: %w", err)
							}
						}(p.Text)
					}
				case data.PointTypeDiscardDownload:
					if p.Value != 0 {
						m.setError(nil)
						err := cleanDownloads()
						if err != nil {
							m.setError(fmt.Errorf("Error cleaning downloads: %w", err))
						}
						err = checkDownloads()
						if err != nil {
							m.setError(err)
						}
					}
				case data.PointTypeReboot:
					err := SendNodePoints(m.nc, m.config.ID, data.Points{
						{Time: time.Now(), Type: data.PointTypeReboot, Value: 0},
					}, true)
					if err != nil {
						m.log.Println("Error clearing reboot point: ", err)
					}

					reboot()

				case data.PointTypeRefresh:
					err := SendNodePoints(m.nc, m.config.ID, data.Points{
						{Time: time.Now(), Type: data.PointTypeRefresh, Value: 0},
					}, true)
					if err != nil {
						m.log.Println("Error clearing reboot reboot point: ", err)
					}

					m.setError(nil)
					err = getUpdates()
					if err != nil {
						m.setError(err)
					}

				case data.PointTypePollPeriod:
					checkTickerTime := time.Minute * time.Duration(p.Value)
					checkTicker.Reset(checkTickerTime)

				case data.PointTypeAutoDownload:
					if p.Value == 1 {
						m.setError(nil)
						err := getUpdates()
						if err != nil {
							m.setError(err)
						} else {
							err :=
								autoDownload()
							if err != nil {
								m.setError(err)
							}
						}
					}

				case data.PointTypePrefix:
					m.setError(nil)
					err := cleanDownloads()
					if err != nil {
						m.setError(fmt.Errorf("Error cleaning downloads: %w", err))
					}
					err = checkDownloads()
					if err != nil {
						m.setError(err)
					}
					err = getUpdates()
					if err != nil {
						m.setError(err)
					}
				case data.PointTypeURI:
					m.setError(nil)
					err := getUpdates()
					if err != nil {
						m.setError(err)
					}
				}
			}

		case pts := <-m.newEdgePoints:
			err := data.MergeEdgePoints(pts.ID, pts.Parent, pts.Points, &m.config)
			if err != nil {
				log.Println("error merging new points:", err)
			}

		case <-cDownloadFinished:
			now := time.Now()
			err := checkDownloads()
			if err != nil {
				m.setError(err)
			}

			pts := data.Points{
				{Time: now, Type: data.PointTypeDownloadOS, Text: ""},
				{Time: now, Type: data.PointTypeOSDownloaded, Text: m.config.OSDownloaded},
			}
			err = SendNodePoints(m.nc, m.config.ID, pts, true)
			if err != nil {
				m.log.Println("Error sending node points: ", err)
			}
			m.log.Println("Download process finished")

			if m.config.AutoReboot {
				// make sure points have time to stick
				time.Sleep(2 * time.Second)
				reboot()
			}

		case <-checkTicker.C:
			m.setError(nil)
			err := getUpdates()
			if err != nil {
				m.setError(err)
				break
			}
			if m.config.AutoDownload {
				err := autoDownload()
				if err != nil {
					m.setError(err)
				}
			}
			err = checkDownloads()
			if err != nil {
				m.setError(err)
			}

		case err := <-cSetError:
			m.setError(err)
		}
	}

	close(cDownloadFinished)
	close(cSetError)

	return nil
}

// Stop sends a signal to the Run function to exit
func (m *UpdateClient) Stop(_ error) {
	close(m.stop)
}

// Points is called by the Manager when new points for this
// node are received.
func (m *UpdateClient) Points(nodeID string, points []data.Point) {
	m.newPoints <- NewPoints{nodeID, "", points}
}

// EdgePoints is called by the Manager when new edge points for this
// node are received.
func (m *UpdateClient) EdgePoints(nodeID, parentID string, points []data.Point) {
	m.newEdgePoints <- NewPoints{nodeID, parentID, points}
}

// below is code that used to be in the store and is in process of being
// ported to a client

// StartUpdate starts an update
/*
func StartUpdate(id, url string) error {
	if _, ok := st.updates[id]; ok {
		return fmt.Errorf("Update already in process for dev: %v", id)
	}

	st.updates[id] = time.Now()

	err := st.setSwUpdateState(id, data.SwUpdateState{
		Running: true,
	})

	if err != nil {
		delete(st.updates, id)
		return err
	}

	go func() {
		err := NatsSendFileFromHTTP(st.nc, id, url, func(bytesTx int) {
			err := st.setSwUpdateState(id, data.SwUpdateState{
				Running:     true,
				PercentDone: bytesTx,
			})

			if err != nil {
				log.Println("Error setting update status in DB:", err)
			}
		})

		state := data.SwUpdateState{
			Running: false,
		}

		if err != nil {
			state.Error = "Error updating software"
			state.PercentDone = 0
		} else {
			state.PercentDone = 100
		}

		st.lock.Lock()
		delete(st.updates, id)
		st.lock.Unlock()

		err = st.setSwUpdateState(id, state)
		if err != nil {
			log.Println("Error setting sw update state:", err)
		}
	}()

	return nil
}
*/
