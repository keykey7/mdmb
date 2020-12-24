package main

import (
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	stdlog "log"
	"os"

	"github.com/go-kit/kit/log"
	"github.com/groob/plist"
	"github.com/jessepeterson/cfgprofiles"
	"github.com/jessepeterson/mdmb/internal/device"
	scepclient "github.com/micromdm/scep/client"
)

func main() {
	f := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	// var (
	// 	dbPath = f.String("db", "mdmb.db", "mdmb database file path")
	// )
	f.Usage = func() {
		fmt.Fprintf(f.Output(), "%s [flags] <subcommand> [flags]\n", f.Name())
		fmt.Fprint(f.Output(), "\nFlags:\n")
		f.PrintDefaults()
		fmt.Fprint(f.Output(), "\nSubcommands:\n")
		fmt.Fprintln(f.Output(), "    enroll\tenroll devices into MDM")
	}
	f.Parse(os.Args[1:])

	if len(f.Args()) < 1 {
		fmt.Fprintln(f.Output(), "no subcommand supplied")
		f.Usage()
		os.Exit(2)
	}

	switch f.Args()[0] {
	case "enroll":
		enroll(f.Args()[1:], f.Usage)
	case "help":
		f.Usage()
	default:
		fmt.Fprintf(f.Output(), "invalid subcommand: %s\n", f.Args()[0])
		f.Usage()
		os.Exit(2)
	}
}

func enroll(args []string, usage func()) {
	f := flag.NewFlagSet("enroll", flag.ExitOnError)
	var (
		// enrollType = f.String("type", "profile", "enrollment type")
		// number     = f.Int("n", 1, "number of devices")
		url  = f.String("url", "", "URL pointing to enrollment spec (e.g. profile)")
		file = f.String("file", "", "file of enrollment spec (e.g. profile)")
	)
	f.Usage = func() {
		usage()
		fmt.Fprintf(f.Output(), "\nFlags for %s subcommand:\n", f.Name())
		f.PrintDefaults()
	}
	f.Parse(args)

	if (*url == "" && *file == "") || (*url != "" && *file != "") {
		fmt.Fprintln(f.Output(), "must specify one enrollment url or file")
		f.Usage()
		os.Exit(2)
	}

	if *url != "" {
		fmt.Fprintln(f.Output(), "-url not yet supported")
		os.Exit(1)
	}

	if err := enrollWithFile(*file); err != nil {
		stdlog.Fatal(err)
	}

	// c := client.NewMDMClient()
	// fmt.Println(c.UDID)
}

func enrollWithFile(path string) error {

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	profile := &cfgprofiles.Profile{}

	dec := plist.NewDecoder(f)
	if err := dec.Decode(profile); err != nil {
		return err
	}

	mdmPlds := profile.MDMPayloads()
	if len(mdmPlds) != 1 {
		return errors.New("invalid number of MDM payloads")
	}
	mdmPld := mdmPlds[0]

	fmt.Printf("CheckIn:\t%s\nConnect:\t%s\n", mdmPld.CheckInURL, mdmPld.ServerURL)

	scepPlds := profile.SCEPPayloads()
	if len(mdmPlds) != 1 {
		return errors.New("invalid number of MDM payloads")
	}
	scepPld := scepPlds[0]

	scepURL := scepPld.PayloadContent.URL
	fmt.Printf("SCEP URL:\t%s\n", scepURL)

	logger := log.NewLogfmtLogger(os.Stderr)
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	cl, err := scepclient.New(scepURL, logger)
	if err != nil {
		return err
	}
	// fmt.Println(cl.Supports("POSTPKIOperation"))
	fmt.Println(cl)

	dev := &device.Device{
		UDID:         "475F0A29-6FCE-419E-A30F-9FF616FD2B87",
		Serial:       "P3IJDS49Z90A",
		ComputerName: "Malik's computer",
	}

	dev.DeviceIdentityKey, err = keyFromSCEPProfilePayload(scepPld, rand.Reader)
	if err != nil {
		return err
	}

	csrBytes, err := csrFromSCEPProfilePayload(scepPld, dev, rand.Reader)
	if err != nil {
		return err
	}

	err = writeCSR(csrBytes, "/tmp/csr.pem")
	if err != nil {
		return err
	}
	fmt.Println("saved CSR to /tmp/csr.pem")

	return nil
}

func writeCSR(csr []byte, filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	pemBlock := &pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csr,
	}
	err = pem.Encode(f, pemBlock)
	if err != nil {
		return err
	}
	return nil
}
