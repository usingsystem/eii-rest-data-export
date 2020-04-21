package main

import (
	eismsgbus "EISMessageBus/eismsgbus"
	configmgr "ConfigManager"
	util "IEdgeInsights/common/util"
	envconfig "EnvConfig"
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

type restExport struct {
	rdeCaCertPool  *x509.CertPool
	extCaCertPool  *x509.CertPool
	clientCert     tls.Certificate
	rdeConfig      map[string]interface{}
	imgStoreConfig map[string]interface{}
	service        *eismsgbus.ServiceRequester
	host           string
	port           string
	devMode        bool
}

const (
	rdeCertPath = "/opt/intel/eis/rde_server_cert.der"
	rdeKeyPath  = "/opt/intel/eis/rde_server_key.der"
)

// init is used to initialize and fetch required config
func (r *restExport) init() {

	flag.Parse()
	flag.Set("logtostderr", "true")
	defer glog.Flush()

	appName := os.Getenv("AppName")
	config := util.GetCryptoMap(appName)
	confHandler := configmgr.Init("etcd", config)
	if confHandler == nil {
		glog.Fatal("Config Manager Initializtion Failed...")
	}

	flag.Set("stderrthreshold", os.Getenv("GO_LOG_LEVEL"))
	flag.Set("v", os.Getenv("GO_VERBOSE"))

	// Setting devMode
	devMode, err := strconv.ParseBool(os.Getenv("DEV_MODE"))
	if err != nil {
		glog.Errorf("string to bool conversion error")
		os.Exit(1)
	}
	r.devMode = devMode

	// Fetching required etcd config
	value, err := confHandler.GetConfig("/" + appName + "/config")
	if err != nil {
		glog.Errorf("Error while fetching config : %s\n", err.Error())
		os.Exit(1)
	}

	// Reading schema json
	schema, err := ioutil.ReadFile("./schema.json")
	if err != nil {
		glog.Errorf("Schema file not found")
		os.Exit(1)
	}

	// Validating config json
	if util.ValidateJSON(string(schema), value) != true {
		glog.Errorf("Error while validating JSON\n")
		os.Exit(1)
	}

	s := strings.NewReader(value)
	err = json.NewDecoder(s).Decode(&r.rdeConfig)
	if err != nil {
		glog.Errorf("Error while decoding JSON : %s\n", err.Error())
		os.Exit(1)
	}

	// Setting host and port of RestExport server
	r.host = fmt.Sprintf("%v", r.rdeConfig["rest_export_server_host"])
	r.port = fmt.Sprintf("%v", r.rdeConfig["rest_export_server_port"])

	// Fetching ImageStore config
	imgStoreConfig := envconfig.GetMessageBusConfig("ImageStore", "client", r.devMode, config)
	r.imgStoreConfig = imgStoreConfig

	// Getting required certs from etcd
	if !r.devMode {

		rdeCerts := []string{rdeCertPath, rdeKeyPath}
		rdeExportKeys := []string{"/RestDataExport/server_cert", "/RestDataExport/server_key"}

		i := 0
		for _, rdeExportKey := range rdeExportKeys {
			rdeCertFile, err := confHandler.GetConfig(rdeExportKey)
			if err != nil {
				glog.Errorf("Error : %s", err)
			}
			certFile := []byte(rdeCertFile)
			err = ioutil.WriteFile(rdeCerts[i], certFile, 0644)
			i++
		}

		// Fetching and storing required CA certs
		serverCaPath := fmt.Sprintf("%v", r.rdeConfig["http_server_ca"])

		caCert, err := ioutil.ReadFile(serverCaPath)
		if err != nil {
			glog.Errorf("Error : %s", err)
		}

		rdeCaFile, err := confHandler.GetConfig("/RestDataExport/ca_cert")
		if err != nil {
			glog.Errorf("Error : %s", err)
		}
		caFile := []byte(rdeCaFile)

		// Adding Rest Data Export and server CA to certificate pool
		extCaCertPool := x509.NewCertPool()
		extCaCertPool.AppendCertsFromPEM(caCert)

		rdeCaCertPool := x509.NewCertPool()
		rdeCaCertPool.AppendCertsFromPEM(caFile)

		r.rdeCaCertPool = rdeCaCertPool
		r.extCaCertPool = extCaCertPool

		// Read the key pair to create certificate struct
		certFile, err := confHandler.GetConfig("/RestDataExport/server_cert")
		if err != nil {
			glog.Errorf("Error : %s", err)
		}
		rdeCertFile := []byte(certFile)

		keyFile, err := confHandler.GetConfig("/RestDataExport/server_key")
		if err != nil {
			glog.Errorf("Error : %s", err)
		}
		rdeKeyFile := []byte(keyFile)

		cert, err := tls.X509KeyPair(rdeCertFile, rdeKeyFile)
		if err != nil {
			glog.Errorf("Error : %s", err)
		}
		r.clientCert = cert
	}

	// Starting EISMbus subcribers
	var subTopics []string
	subTopics = envconfig.GetTopics("SUB")
	for _, subTopicCfg := range subTopics {
		msgBusConfig := envconfig.GetMessageBusConfig(subTopicCfg, "SUB", r.devMode, config)
		subTopicCfg := strings.Split(subTopicCfg, "/")
		go r.startEisSubscriber(msgBusConfig, subTopicCfg[1])
	}

	client, err := eismsgbus.NewMsgbusClient(r.imgStoreConfig)
	if err != nil {
		glog.Errorf("-- Error initializing message bus context: %v\n", err)
	}

	service, err := client.GetService("ImageStore")
	if err != nil {
		glog.Errorf("-- Error initializing service requester: %v\n", err)
	}
	r.service = service

}

// startEisSubscriber is used to start EISMbus subscribers over specified topic
func (r *restExport) startEisSubscriber(config map[string]interface{}, topic string) {

	client, err := eismsgbus.NewMsgbusClient(config)
	if err != nil {
		glog.Errorf("-- Error initializing message bus context: %v\n", err)
		return
	}
	defer client.Close()
	subscriber, err := client.NewSubscriber(topic)
	if err != nil {
		glog.Errorf("-- Error subscribing to topic: %v\n", err)
		return
	}
	defer subscriber.Close()

	for {
		select {
		case msg := <-subscriber.MessageChannel:
			glog.V(1).Infof("-- Received Message --")
			// Adding topic to meta-data for easy differentitation in external server
			msg.Data["topic"] = topic
			r.publishMetaData(msg.Data, topic)
		case err := <-subscriber.ErrorChannel:
			glog.Errorf("Error receiving message: %v\n", err)
		}
	}
}

// publishMetaData is used to send metadata via POST requests to external server
func (r *restExport) publishMetaData(metadata map[string]interface{}, topic string) {

	// Adding meta-data to http request
	requestBody, err := json.Marshal(metadata)
	if err != nil {
		glog.Errorf("Error marshalling json : %s", err)
	}

	// Timeout for every request
	timeout := time.Duration(60 * time.Second)

	if r.devMode {

		client := &http.Client{
			Timeout: timeout,
		}

		// Getting endpoint of server
		endpoint := fmt.Sprintf("%v", r.rdeConfig[topic])

		// Making a post request to external server
		r, err := client.Post(endpoint+"/metadata", "application/json", bytes.NewBuffer(requestBody))
		if err != nil {
			glog.Errorf("Remote HTTP server is not responding : %s", err)
		}

		// Read the response body
		defer r.Body.Close()
		response, err := ioutil.ReadAll(r.Body)
		if err != nil {
			glog.Errorf("Failed to receive response from server : %s", err)
		}

		glog.Infof("Response : %s", string(response))

	} else {

		// Getting endpoint of server
		endpoint := fmt.Sprintf("%v", r.rdeConfig[topic])

		// Create a HTTPS client and supply the created CA pool and certificate
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:      r.extCaCertPool,
					Certificates: []tls.Certificate{r.clientCert},
				},
			},
			Timeout: timeout,
		}

		// Replace http with https for PROD mode
		endpoint = strings.Replace(endpoint, "http", "https", 1)
		// Making a post request to external server
		r, err := client.Post(endpoint+"/metadata", "application/json", bytes.NewBuffer(requestBody))
		if err != nil {
			glog.Errorf("Remote HTTP server is not responding : %s", err)
		}

		// Read the response body
		defer r.Body.Close()
		response, err := ioutil.ReadAll(r.Body)
		if err != nil {
			glog.Errorf("Failed to receive response from server : %s", err)
		}

		glog.Infof("Response : %s", string(response))

	}
}

// restExportServer starts a http server to serve GET requests
func (r *restExport) restExportServer() {

	http.HandleFunc("/image", r.getImage)

	if r.devMode {
		err := http.ListenAndServe(r.host+":"+r.port, nil)
		if err != nil {
			glog.Errorf("%v", err)
			os.Exit(-1)
		}
	} else {

		// Create the TLS Config with the CA pool and enable Client certificate validation
		tlsConfig := &tls.Config{
			RootCAs:    r.rdeCaCertPool,
			ClientCAs:  r.extCaCertPool,
			ClientAuth: tls.RequireAndVerifyClientCert,
		}
		tlsConfig.BuildNameToCertificate()

		// Create a Server instance to listen on port with the TLS config
		server := &http.Server{
			Addr:              r.host + ":" + r.port,
			ReadTimeout:       60 * time.Second,
			ReadHeaderTimeout: 60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    1 << 20,
			TLSConfig:         tlsConfig,
		}

		// Listen to HTTPS connections with the server certificate and wait
		err := server.ListenAndServeTLS(rdeCertPath, rdeKeyPath)
		if err != nil {
			glog.Errorf("%v", err)
			os.Exit(-1)
		}
	}
}

// readImage is used to fetch required image from ImageStore
func (r *restExport) readImage(imgHandle string) []byte {

	// Send Read command & get the frame data
	response := map[string]interface{}{"command": "read", "img_handle": imgHandle}
	err1 := r.service.Request(response)
	if err1 != nil {
		glog.Errorf("-- Error sending request: %v\n", err1)
		return nil
	}

	resp, err := r.service.ReceiveResponse(-1)
	if err != nil {
		glog.Errorf("-- Error receiving response: %v\n", err)
		return nil
	}

	return resp.Blob
}

// getImage publishes image frame via GET request to external server
func (r *restExport) getImage(w http.ResponseWriter, re *http.Request) {
	// Setting content type for encoding
	w.Header().Set("Content-type", "image/jpeg; charset=utf-8")

	switch re.Method {
	case "GET":
		w.WriteHeader(http.StatusOK)
		imgHandle := strings.Split(re.URL.RawQuery, "=")[1]
		// Send imgHandle to read from ImageStore
		frame := r.readImage(imgHandle)
		glog.Infof("Imghandle %s and md5sum %v", imgHandle, md5.Sum(frame))
		w.Write(frame)
	case "POST":
		fmt.Fprintf(w, "Received a POST request")
	default:
		fmt.Fprintf(w, "Sorry, only GET and POST methods are supported.")
	}
}

func main() {

	// initializing constructor
	r := new(restExport)
	r.init()

	// start the Rest Export server to serve images via GET requests
	go r.restExportServer()

	select {}
}
