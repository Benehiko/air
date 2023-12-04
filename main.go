package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/gousb"
	"github.com/gorilla/mux"
	errorsx "github.com/ory/x/errorsx"
	"github.com/phayes/freeport"
	"github.com/urfave/negroni"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/Masterminds/sprig/v3"
	"github.com/spf13/cobra"
)

//go:embed templates/*
var templates embed.FS

type AirQuality struct {
	ID        int       `json:"id"`
	PM25      float64   `json:"pm25"`
	PM10      float64   `json:"pm10"`
	CreatedAt time.Time `json:"created_at"`
}

func openDatabase() error {
	newDB, err := bolt.Open("airquality.db", 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not open database: %+v", err))
	}
	db.db = newDB
	return nil
}

func sensorHandler(ctx context.Context, cmd *cobra.Command, log *zap.SugaredLogger) error {
	log.Info("Starting sensor handler")

	deviceCtx := gousb.NewContext()
	defer deviceCtx.Close()

	log.Info("Opening device 1a86:7523")
	device, err := deviceCtx.OpenDeviceWithVIDPID(0x1a86, 0x7523)
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not open a device: %v", err))
	}
	defer device.Close()

	log.Info("Setting auto detach")
	// we need this to detach the device from the kernel
	if err := device.SetAutoDetach(true); err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not set auto detach: %v", err))
	}

	log.Info("Attempting to reset the device")
	// reset the device
	if err := device.Reset(); err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not reset device: %v", err))
	}

	cfg, err := device.Config(1)
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not get config: %v", err))
	}
	defer cfg.Close()

	intf, err := cfg.Interface(0, 0)
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not get interface: %v", err))
	}

	epIn, err := intf.InEndpoint(2)
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not get IN endpoint: %v", err))
	}

	log.Info("Starting read cycle with a 30 second delay between reads")
	now := time.Now()

	stream, err := epIn.NewStream(epIn.Desc.MaxPacketSize, 10)
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not create stream: %v", err))
	}
	defer stream.Close()

	for {
	next:
		switch {
		case time.Since(now) >= 30*time.Second:
			now = time.Now()
			buf := make([]byte, 10*epIn.Desc.MaxPacketSize)
			totalRead, err := stream.ReadContext(ctx, buf)
			if err != nil {
				log.Errorf("Read returned an error: %v", errorsx.WithStack(err))
				goto next
			}
			if totalRead == 0 {
				return errorsx.WithStack(fmt.Errorf("IN endpoint 2 returned 0 bytes of data."))
			}

			if buf[0] != 0xAA || buf[1] != 0xC0 {
				log.Error(errorsx.WithStack(fmt.Errorf("Invalid header: %d %d", buf[0], buf[1])))
				goto next
			}

			checksum := (buf[2] + buf[3] + buf[4] + buf[5] + buf[6] + buf[7]) & 0xFF
			if checksum != buf[8] {
				log.Error(errorsx.WithStack(fmt.Errorf("Checksum error: %d != %d", checksum, buf[6])))
				goto next
			}

			pm25 := float64((buf[3]*0xFF)+buf[2]) / 10.0
			pm10 := float64((buf[5]*0xFF)+buf[4]) / 10.0
			log.Infof("pm2.5: %.2f", pm25)
			log.Infof("pm10: %.2f", pm10)

			err = db.db.Update(func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists([]byte("airquality"))
				if err != nil {
					return err
				}
				// Generate ID for the user.
				// This returns an error only if the Tx is closed or not writeable.
				// That can't happen in an Update() call so I ignore the error check.
				id, _ := b.NextSequence()
				now := time.Now().UTC()
				data, err := json.Marshal(AirQuality{
					ID:        int(id),
					PM25:      pm25,
					PM10:      pm10,
					CreatedAt: now,
				})
				if err != nil {
					return err
				}

				return b.Put([]byte(now.Format(time.RFC3339)), data)
			})

			if err != nil {
				log.Error(errorsx.WithStack(fmt.Errorf("Could not write to database: %+v", err)))
			}
			log.Info("Successfully wrote to database")
		default:
			time.Sleep(30 * time.Second)
		}
	}
}

func webHandler(ctx context.Context, cmd *cobra.Command, log *zap.SugaredLogger) error {
	log.Info("Starting web handler")

	var port int
	var err error
	portStr, err := cmd.Flags().GetString("port")
	if err != nil {
		port, err = freeport.GetFreePort()
		if err != nil {
			return errorsx.WithStack(fmt.Errorf("Could not get free port: %+v", err))
		}
	} else {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return errorsx.WithStack(fmt.Errorf("Could not convert port to int: %+v", err))
		}
	}

	r := mux.NewRouter()
	n := negroni.New()

	static, err := fs.Sub(fs.FS(templates), "templates")
	if err != nil {
		log.Error(errorsx.WithStack(fmt.Errorf("Could not get CSS subdirectory: %+v", err)))
	}
	r.Handle("/static", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	sub := r.PathPrefix("/static").Subrouter()
	sub.PathPrefix("/css").Handler(http.StripPrefix("/static/css", http.FileServer(http.FS(static))))
	sub.PathPrefix("/js").Handler(http.StripPrefix("/static/js", http.FileServer(http.FS(static))))

	r.HandleFunc("/data-options", func(w http.ResponseWriter, r *http.Request) {
		var data []string
		err := db.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("airquality"))
			c := b.Cursor()

			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				data = append(data, string(k))
			}

			return nil
		})
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not read from database: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		templ, err := template.New("options.html").Funcs(sprig.HtmlFuncMap()).ParseFS(static, "options.html")
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not parse template: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		err = templ.Execute(w, data)
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not execute template: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})
	r.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		var data []AirQuality
		err := db.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("airquality"))
			c := b.Cursor()

			startTime := time.Now().UTC().Add(-15 * time.Minute)
			endTime := time.Now().UTC()

			start := r.URL.Query().Get("start")
			if start != "" {
				startTime, err = time.Parse(time.RFC3339, start)
				if err != nil {
					return errorsx.WithStack(fmt.Errorf("Could not parse start time: %+v", err))
				}
			}

			end := r.URL.Query().Get("end")
			if end != "" {
				endTime, err = time.Parse(time.RFC3339, end)
				if err != nil {
					return errorsx.WithStack(fmt.Errorf("Could not parse end time: %+v", err))
				}
			}

			for k, v := c.Seek([]byte(startTime.Format(time.RFC3339))); k != nil && bytes.Compare(k, []byte(endTime.Format(time.RFC3339))) <= 0; k, v = c.Next() {
				var aq AirQuality
				err := json.Unmarshal(v, &aq)
				if err != nil {
					return err
				}
				data = append(data, aq)
			}

			return nil
		})
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not read from database: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		sort.Slice(data, func(i, j int) bool {
			return data[i].ID < data[j].ID
		})

		templ, err := template.New("chart.html").Funcs(sprig.HtmlFuncMap()).ParseFS(static, "chart.html")
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not parse template: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		err = templ.Execute(w, data)
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not execute template: %+v", err)))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.New("index.html").Funcs(sprig.HtmlFuncMap()).ParseFS(static, "index.html")
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not parse template: %+v", err)))
			return
		}
		err = tmpl.Execute(w, nil)
		if err != nil {
			log.Error(errorsx.WithStack(fmt.Errorf("Could not execute template: %+v", err)))
			return
		}
	})

	n.Use(negroni.NewRecovery())
	n.UseHandler(r)

	log.Infof("Starting server on port %d...", port)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), n); err != nil {
		log.Errorf("Error starting server: %s", err.Error())
		return err
	}
	return nil
}

type BoltDB struct {
	db *bolt.DB
}

var db *BoltDB

func main() {
	db = &BoltDB{
		db: nil,
	}

	if err := openDatabase(); err != nil {
		panic(err)
	}

	// WORKAROUND annoying gousb logging
	const interruptedError = "handle_events: error: libusb: interrupted [code -10]"

	logger, err := zap.NewProduction()
	if err != nil {
		panic(fmt.Errorf("Could not create logger: %+v", err))
	}
	// flushes buffer, if any
	defer logger.Sync()
	// redirect stdlib logging to zap
	undoRedirect := zap.RedirectStdLog(logger)
	defer undoRedirect()

	sugar := logger.Sugar()
	root := cobra.Command{}

	allCmd := cobra.Command{
		Use:   "all",
		Short: "Starts the sensor and the web dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			g, ctx := errgroup.WithContext(cmd.Context())

			g.Go(func() error {
				return sensorHandler(ctx, cmd, sugar)
			})

			g.Go(func() error {
				return webHandler(ctx, cmd, sugar)
			})

			if err := g.Wait(); err != nil {
				panic(fmt.Errorf("Could not start sensor handler: %+v", err))
			}

			return nil
		},
	}

	allCmd.PersistentFlags().StringP("port", "p", "", "Port to listen on")

	webCmd := cobra.Command{
		Use:   "web",
		Short: "Starts the web dashboard",

		RunE: func(cmd *cobra.Command, args []string) error {
			return webHandler(cmd.Context(), cmd, sugar)
		},
	}

	webCmd.PersistentFlags().StringP("port", "p", "", "Port to listen on")

	sensorCmd := cobra.Command{
		Use:   "sensor",
		Short: "Starts the sensor",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sensorHandler(cmd.Context(), cmd, sugar)
		},
	}

	root.AddCommand(&sensorCmd)
	root.AddCommand(&webCmd)
	root.AddCommand(&allCmd)

	if err := root.Execute(); err != nil {
		panic(fmt.Errorf("Could not execute root command: %+v", err))
	}
}
