package main

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/pkcs12"

	"github.com/RobotsAndPencils/buford/push"
	"github.com/boltdb/bolt"
	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
	httptransport "github.com/go-kit/kit/transport/http"
	"github.com/gorilla/mux"

	boltdepot "github.com/micromdm/scep/depot/bolt"
	scep "github.com/micromdm/scep/server"

	"github.com/micromdm/nano/checkin"
	"github.com/micromdm/nano/command"
	"github.com/micromdm/nano/connect"
	"github.com/micromdm/nano/device"
	"github.com/micromdm/nano/enroll"
	"github.com/micromdm/nano/pubsub"
	nanopush "github.com/micromdm/nano/push"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func main() {
	var (
		flServerURL    = flag.String("server.url", "", "public HTTPS url of your server")
		flAPNSCertPath = flag.String("apns.certificate", "mdm.p12", "path to APNS certificate")
		flAPNSKeyPass  = flag.String("apns.password", "secret", "password for your APNS cert file.")
		flAPNSKeyPath  = flag.String("apns.key", "", "path to key file if using .pem push cert")
	)
	flag.Parse()

	logger := log.NewLogfmtLogger(os.Stderr)
	stdlog.SetOutput(log.NewStdlibAdapter(logger)) // force structured logs
	mainLogger := log.NewContext(logger).With("component", "main")
	mainLogger.Log("msg", "started")

	sm := &config{
		ServerPublicURL:     *flServerURL,
		APNSCertificatePath: *flAPNSCertPath,
		APNSPrivateKeyPass:  *flAPNSKeyPass,
		APNSPrivateKeyPath:  *flAPNSKeyPath,
	}
	sm.setupPubSub()
	sm.setupBolt()
	sm.loadPushCerts()
	sm.setupSCEP()
	sm.setupEnrollmentService()
	sm.setupCheckinService()
	sm.setupPushService()
	sm.setupCommandService()
	sm.setupCommandQueue()
	if sm.err != nil {
		stdlog.Fatal(sm.err)
	}

	_, err := device.NewDB(sm.db, sm.pubclient)
	if err != nil {
		stdlog.Fatal(sm.err)
	}

	ctx := context.Background()
	httpLogger := log.NewContext(logger).With("transport", "http")
	var checkinEndpoint endpoint.Endpoint
	{
		checkinEndpoint = checkin.MakeCheckinEndpoint(sm.checkinService)
	}

	checkinEndpoints := checkin.Endpoints{
		CheckinEndpoint: checkinEndpoint,
	}

	checkinOpts := []httptransport.ServerOption{
		httptransport.ServerErrorLogger(httpLogger),
		httptransport.ServerErrorEncoder(checkin.EncodeError),
	}
	checkinHandlers := checkin.MakeHTTPHandlers(ctx, checkinEndpoints, checkinOpts...)

	pushEndpoints := nanopush.Endpoints{
		PushEndpoint: nanopush.MakePushEndpoint(sm.pushService),
	}

	commandEndpoints := command.Endpoints{
		NewCommandEndpoint: command.MakeNewCommandEndpoint(sm.commandService),
	}

	commandHandlers := command.MakeHTTPHandlers(ctx, commandEndpoints, checkinOpts...)

	pushHandlers := nanopush.MakeHTTPHandlers(ctx, pushEndpoints, checkinOpts...)
	scepHandler := scep.ServiceHandler(ctx, sm.scepService, httpLogger)
	enrollHandler := enroll.ServiceHandler(ctx, sm.enrollService, httpLogger)
	r := mux.NewRouter()
	r.Handle("/mdm/checkin", checkinHandlers.CheckinHandler).Methods("PUT")
	r.Handle("/mdm/enroll", enrollHandler)
	r.Handle("/scep", scepHandler)
	r.Handle("/push/{udid}", pushHandlers.PushHandler)
	r.Handle("/v1/commands", commandHandlers.NewCommandHandler).Methods("POST")

	errs := make(chan error, 2)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT)
		errs <- fmt.Errorf("%s", <-c)
	}()

	go func() {
		var httpAddr = "0.0.0.0:8080"
		logger := log.NewContext(logger).With("transport", "HTTP")
		logger.Log("addr", httpAddr)
		errs <- http.ListenAndServe(
			httpAddr, r)
	}()
	mainLogger.Log("terminated", <-errs)
}

type config struct {
	pubclient           *pubsub.Inmem
	db                  *bolt.DB
	pushCert            pushServiceCert
	ServerPublicURL     string
	SCEPChallenge       string
	APNSPrivateKeyPath  string
	APNSCertificatePath string
	APNSPrivateKeyPass  string

	PushService    *push.Service // bufford push
	pushService    *nanopush.Push
	checkinService checkin.Service
	enrollService  enroll.Service
	scepService    scep.Service
	commandService command.Service

	err error
}

func (c *config) setupPubSub() {
	if c.err != nil {
		return
	}
	c.pubclient = pubsub.NewInmemPubsub()
}

func (c *config) setupCommandService() {
	if c.err != nil {
		return
	}
	c.commandService, c.err = command.New(c.db, c.pubclient)
}

func (c *config) setupCommandQueue() {
	if c.err != nil {
		return
	}
	_, err := connect.NewQueue(c.db, c.pubclient)
	if err != nil {
		c.err = err
	}
}

func (c *config) setupCheckinService() {
	if c.err != nil {
		return
	}
	c.checkinService, c.err = checkin.New(c.db, c.pubclient)
}

func (c *config) setupBolt() {
	if c.err != nil {
		return
	}
	c.db, c.err = bolt.Open("mdm.db", 0777, nil)
	if c.err != nil {
		return
	}
}

func (c *config) loadPushCerts() {
	if c.err != nil {
		return
	}

	if c.APNSPrivateKeyPath == "" {
		var pkcs12Data []byte
		pkcs12Data, c.err = ioutil.ReadFile(c.APNSCertificatePath)
		if c.err != nil {
			return
		}
		c.pushCert.PrivateKey, c.pushCert.Certificate, c.err =
			pkcs12.Decode(pkcs12Data, c.APNSPrivateKeyPass)
		return
	}

	var pemData []byte
	pemData, c.err = ioutil.ReadFile(c.APNSCertificatePath)
	if c.err != nil {
		return
	}

	pemBlock, _ := pem.Decode(pemData)
	if pemBlock == nil {
		c.err = errors.New("invalid PEM data for cert")
		return
	}
	c.pushCert.Certificate, c.err = x509.ParseCertificate(pemBlock.Bytes)
	if c.err != nil {
		return
	}

	pemData, c.err = ioutil.ReadFile(c.APNSPrivateKeyPath)
	if c.err != nil {
		return
	}

	pemBlock, _ = pem.Decode(pemData)
	if pemBlock == nil {
		c.err = errors.New("invalid PEM data for privkey")
		return
	}
	c.pushCert.PrivateKey, c.err =
		x509.ParsePKCS1PrivateKey(pemBlock.Bytes)
}

type pushServiceCert struct {
	*x509.Certificate
	PrivateKey interface{}
}

func (c *config) setupPushService() {
	if c.err != nil {
		return
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{c.pushCert.Certificate.Raw},
		PrivateKey:  c.pushCert.PrivateKey,
		Leaf:        c.pushCert.Certificate,
	}
	client, err := push.NewClient(tlsCert)
	if err != nil {
		c.err = err
		return
	}
	c.PushService = &push.Service{
		Client: client,
		Host:   push.Production,
	}

	db, err := nanopush.NewDB(c.db, c.pubclient)
	if err != nil {
		c.err = err
		return
	}
	c.pushService = nanopush.New(db, c.PushService)
}

func (c *config) setupEnrollmentService() {
	if c.err != nil {
		return
	}
	pushTopic, err := topicFromCert(c.pushCert.Certificate)
	if err != nil {
		c.err = err
		return
	}
	pub, err := url.Parse(c.ServerPublicURL)
	if err != nil {
		c.err = err
		return
	}
	SCEPRemoteURL := "https://" + strings.Split(pub.Host, ":")[0] + "/scep"

	var tlsCert string
	var SCEPCertificateSubject string
	// TODO: clean up order of inputs. Maybe pass *SCEPConfig as an arg?
	// but if you do, the packages are coupled, better not.
	c.enrollService, c.err = enroll.NewService(
		pushTopic,
		scepCACertName,
		SCEPRemoteURL,
		c.SCEPChallenge,
		c.ServerPublicURL,
		tlsCert,
		SCEPCertificateSubject,
	)
}

func topicFromCert(cert *x509.Certificate) (string, error) {
	var oidASN1UserID = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}
	for _, v := range cert.Subject.Names {
		if v.Type.Equal(oidASN1UserID) {
			return v.Value.(string), nil
		}
	}

	return "", errors.New("could not find Push Topic (UserID OID) in certificate")
}

const scepCACertName = "SCEPCACert.pem"

func (c *config) setupSCEP() {
	if c.err != nil {
		return
	}

	depot, err := boltdepot.NewBoltDepot(c.db)
	if err != nil {
		c.err = err
		return
	}

	key, err := depot.CreateOrLoadKey(2048)
	if err != nil {
		c.err = err
		return
	}

	caCert, err := depot.CreateOrLoadCA(key, 5, "MicroMDM", "US")
	if err != nil {
		c.err = err
		return
	}

	c.err = savePEMCert(scepCACertName, caCert)
	if c.err != nil {
		return
	}

	opts := []scep.ServiceOption{
		scep.ClientValidity(365),
	}
	c.scepService, c.err = scep.NewService(depot, opts...)
}

func savePEMKey(path string, key *rsa.PrivateKey) error {
	keyOutput, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600,
	)
	if err != nil {
		return err
	}
	defer keyOutput.Close()

	return pem.Encode(
		keyOutput,
		&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
}

func savePEMCert(path string, cert *x509.Certificate) error {
	certOutput, err := os.Create(path)
	if err != nil {
		return err
	}
	defer certOutput.Close()

	return pem.Encode(
		certOutput,
		&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert.Raw,
		})
}
