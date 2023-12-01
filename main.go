package main

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/google/gousb"
	"github.com/gorilla/mux"
	errorsx "github.com/ory/x/errorsx"
	"github.com/phayes/freeport"
	"github.com/urfave/negroni"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

//go:embed templates/*
var templates embed.FS

type AirQuality struct {
	ID        int       `json:"id"`
	PM25      float64   `json:"pm25"`
	PM10      float64   `json:"pm10"`
	CreatedAt time.Time `json:"created_at"`
}

func sensorHandler(ctx context.Context, db *bolt.DB, log *zap.SugaredLogger) error {
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

	log.Info("Starting read cycle with a 5 second delay between reads")
	now := time.Now()

	buf := make([]byte, 10*epIn.Desc.MaxPacketSize)
	for {
		switch {
		case time.Since(now) > 5*time.Second:
			// Repeat the read/write cycle 10 times.
			for i := 0; i < 10; i++ {
				// readBytes might be smaller than the buffer size. readBytes might be greater than zero even if err is not nil.
				readBytes, err := epIn.Read(buf)
				if err != nil {
					log.Errorf("Read returned an error: %v", err)
					return errorsx.WithStack(fmt.Errorf("Read returned an error: %v", err))
				}
				if readBytes == 0 {
					return errorsx.WithStack(fmt.Errorf("IN endpoint 2 returned 0 bytes of data."))
				}
			}

			if buf[0] != 0xAA || buf[1] != 0xC0 {
				return errorsx.WithStack(fmt.Errorf("Invalid header: %d %d", buf[0], buf[1]))
			}

			checksum := (buf[2] + buf[3] + buf[4] + buf[5] + buf[6] + buf[7]) & 0xFF
			if checksum != buf[8] {
				return errorsx.WithStack(fmt.Errorf("Checksum error: %d != %d", checksum, buf[6]))
			}

			pm25 := float64((buf[3]*0xFF)+buf[2]) / 10.0
			pm10 := float64((buf[5]*0xFF)+buf[4]) / 10.0
			log.Infof("pm2.5: %.2f", pm25)
			log.Infof("pm10: %.2f", pm10)

			err := db.Update(func(tx *bolt.Tx) error {
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
				idb := make([]byte, 8)
				binary.LittleEndian.PutUint64(idb, uint64(id))
				return b.Put(idb, data)
			})
			if err != nil {
				return errorsx.WithStack(fmt.Errorf("Could not write to database: %+v", err))
			}
			now = time.Now()
			// reset the buffer with the same memory allocation
			buf = buf[:0]
		default:
			time.Sleep(5 * time.Second)
		}
	}
}

func webHandler(ctx context.Context, db *bolt.DB, log *zap.SugaredLogger) error {
	log.Info("Starting web handler")

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

	r.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		var data []AirQuality
		err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("airquality"))
			c := b.Cursor()

			for k, v := c.First(); k != nil; k, v = c.Next() {
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

		templ, err := template.ParseFS(static, "chart.html")
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
		tmpl, err := template.ParseFS(static, "index.html")
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

	port, err := freeport.GetFreePort()
	if err != nil {
		return errorsx.WithStack(fmt.Errorf("Could not get free port: %+v", err))
	}

	log.Infof("Starting server on port %d...", port)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), n); err != nil {
		log.Errorf("Error starting server: %s", err.Error())
		return err
	}
	return nil
}

func main() {
	ctx := context.Background()

	logger, err := zap.NewProduction()
	if err != nil {
		panic(fmt.Errorf("Could not create logger: %+v", err))
	}
	// flushes buffer, if any
	defer logger.Sync()

	sugar := logger.Sugar()

	sugar.Info("Opening database")

	path := "airquality.db"
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		panic(fmt.Errorf("Could not open database: %+v", err))
	}
	defer db.Close()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return sensorHandler(ctx, db, sugar)
	})

	g.Go(func() error {
		return webHandler(ctx, db, sugar)
	})

	if err := g.Wait(); err != nil {
		panic(fmt.Errorf("Could not start sensor handler: %+v", err))
	}
}
