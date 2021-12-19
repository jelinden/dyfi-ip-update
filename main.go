package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

var outboundIP string
var quit = make(chan struct{})
var client = &http.Client{}

const portfolioURL = "jelinden-portfolio.dy.fi"
const newsURL = "jelinden.dy.fi"

var dyUsername, dyPassword string

func main() {
	dyUsername = os.Getenv("dyUsername")
	dyPassword = os.Getenv("dyPassword")
	outboundIP = GetOutboundIP().To4().String()
	log.Println(outboundIP)
	go heartBeat(checkIPchanged, 3*time.Second)
	go heartBeatWithParams(updateIP, 24*5*time.Hour, portfolioURL)
	go heartBeatWithParams(updateIP, 24*5*time.Hour, newsURL)
	<-quit // use close(quit) to exit
}

func checkIPchanged() {
	ip := GetOutboundIP()
	if ip != nil && ip.To4().String() != outboundIP {
		log.Println("changing ip from", outboundIP, "to", ip.To4().String())
		outboundIP = ip.To4().String()
		updateIP(portfolioURL)
		updateIP(newsURL)
	}
}

func updateIP(hostname string) {
	req, err := http.NewRequest("GET", "https://www.dy.fi/nic/update", nil)
	if err != nil {
		log.Println(err)
	}
	q := url.Values{}
	q.Add("hostname", hostname)
	req.URL.RawQuery = q.Encode()

	req.SetBasicAuth(dyUsername, dyPassword)
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
	}
	fmt.Println(hostname, "\n", string(body))
}

func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Println(err)
		return nil
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}

func heartBeat(what func(), value time.Duration) {
	for range time.Tick(time.Duration(value)) {
		what()
	}
}

func heartBeatWithParams(what func(param string), value time.Duration, param string) {
	for range time.Tick(time.Duration(value)) {
		what(param)
	}
}
