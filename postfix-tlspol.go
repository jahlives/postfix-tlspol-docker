/*
 * MIT License
 * Copyright (c) 2024 Zuplu
 */

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/govalidator"
	"github.com/go-redis/redis/v8"
	"github.com/miekg/dns"
	"gopkg.in/yaml.v2"
)

type ServerConfig struct {
	Address string `yaml:"address"`
}

type DNSConfig struct {
	Address string `yaml:"address"`
}

type RedisConfig struct {
	Address  string `yaml:"address"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type Config struct {
	Server ServerConfig `yaml:"server"`
	DNS    DNSConfig    `yaml:"dns"`
	Redis  RedisConfig  `yaml:"redis"`
}

const (
	CACHE_KEY_PREFIX = "TLSPOL-"
	CACHE_MIN        = 300
	DNS_TIMEOUT      = 5 * time.Second
)

var (
	ctx    = context.Background()
	config Config
	client *redis.Client
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: postfix-tlspol <config.yaml>")
		return
	}

	configFile := os.Args[1]

	var err error
	config, err = loadConfig(configFile)
	if err != nil {
		fmt.Println("Error loading config:", err)
		return
	}

	client = redis.NewClient(&redis.Options{
		Addr:     config.Redis.Address,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	})

	go startTCPServer()

	// Keep the main function alive
	select {}
}

func startTCPServer() {
	listener, err := net.Listen("tcp", config.Server.Address)
	if err != nil {
		fmt.Println("Error starting TCP server:", err)
		return
	}
	defer listener.Close()

	fmt.Printf("Listening on %s...\n", config.Server.Address)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	// Read the incoming query
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		fmt.Println("Error reading from connection:", err)
		return
	}

	query := strings.TrimSpace(string(buffer[:n]))

	parts := strings.Split(query, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		subParts := strings.SplitN(part, ":", 2)
		if len(subParts) > 1 {
			query = strings.TrimSpace(subParts[1])
		}
	}

	parts = strings.SplitN(query, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "query" {
		fmt.Printf("Malformed query: %s\n", query)
		conn.Write([]byte("5:PERM ,"))
		return
	}

	// The domain is the second part
	domain := strings.ToLower(strings.TrimSpace(parts[1]))
	if govalidator.IsIPv4(domain) || govalidator.IsIPv6(domain) {
		fmt.Printf("Skipping policy for non-domain %s\n", domain)
		conn.Write([]byte("9:NOTFOUND ,"))
		return
	}
	if strings.HasPrefix(domain, ".") {
		fmt.Printf("Skipping policy for parent domain %s\n", domain)
		conn.Write([]byte("9:NOTFOUND ,"))
		return
	}

	hashedDomain := sha256.Sum256([]byte(domain))
	cacheKey := CACHE_KEY_PREFIX + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hashedDomain[:])
	cachedResult, err := client.Get(ctx, cacheKey).Result()
	if err == nil {
		if cachedResult == "" {
			fmt.Printf("No policy found for %s (from cache)\n", domain)
			conn.Write([]byte("9:NOTFOUND ,"))

		} else {
			fmt.Printf("Evaluated policy for %s: %s (from cache)\n", domain, cachedResult)
			conn.Write([]byte(fmt.Sprintf("%d:OK %s,", len(cachedResult)+3, cachedResult)))
		}
		return
	}

	var wg sync.WaitGroup

	wg.Add(2)
	result := ""
	var resultTtl int32 = CACHE_MIN
	var daneTtl uint32 = 0
	var mutex sync.Mutex
	go func() {
		defer wg.Done()
		var danePol string
		danePol, daneTtl = checkDANE(domain)
		mutex.Lock()
		if danePol != "" {
			result = danePol
			resultTtl = int32(daneTtl)
		}
		mutex.Unlock()
	}()

	var stsTtl uint32 = 0
	go func() {
		defer wg.Done()
		var stsPol string
		stsPol, stsTtl = checkMTASTS(domain)
		mutex.Lock()
		if stsPol != "" && result == "" {
			result = stsPol
			resultTtl = int32(stsTtl)
		}
		mutex.Unlock()
	}()

	wg.Wait()

	if result == "" {
		resultTtl = CACHE_MIN
		fmt.Printf("No policy found for %s (cached for %ds)\n", domain, resultTtl)
		conn.Write([]byte("9:NOTFOUND ,"))
	} else if result == "TEMP" {
		resultTtl = 10
		fmt.Printf("Evaluating policy for %s failed temporarily (cached for %ds)\n", domain, resultTtl)
		conn.Write([]byte("5:TEMP ,"))
	} else {
		fmt.Printf("Evaluated policy for %s: %s (cached for %ds)\n", domain, result, resultTtl)
		conn.Write([]byte(fmt.Sprintf("%d:OK %s,", len(result)+3, result)))
	}
	client.Set(ctx, cacheKey, result, time.Duration(resultTtl)*time.Second).Err()
}

func loadConfig(filename string) (Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return Config{}, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	return config, err
}

func checkDANE(domain string) (string, uint32) {
	mxRecords, ttl, err := getMXRecords(domain)
	if err != nil {
		return "TEMP", 0
	}
	if len(mxRecords) == 0 {
		return "", 0
	}

	var wg sync.WaitGroup
	tlsaResults := make(chan ResultWithTtl, len(mxRecords))

	for _, mx := range mxRecords {
		wg.Add(1)
		go func(mx string) {
			defer wg.Done()
			tlsaResults <- checkTLSA(mx)
		}(mx)
	}

	wg.Wait()
	close(tlsaResults)

	allHaveTLSA := true
	var ttls []uint32
	ttls = append(ttls, ttl)
	for res := range tlsaResults {
		if res.Err != nil {
			return "TEMP", 0
		}
		ttls = append(ttls, res.Ttl)
		if !res.Result {
			allHaveTLSA = false
		}
	}

	if allHaveTLSA {
		return "dane", findMin(ttls)
	}
	return "", findMin(ttls)
}

func getMXRecords(domain string) ([]string, uint32, error) {
	client := &dns.Client{
		Timeout: DNS_TIMEOUT,
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), dns.TypeMX)
	m.RecursionDesired = true
	m.SetEdns0(4096, true)

	r, _, err := client.Exchange(m, config.DNS.Address)
	if err != nil {
		return nil, 0, err
	}

	if r.Rcode != dns.RcodeSuccess && r.Rcode != dns.RcodeNameError {
		return nil, 0, fmt.Errorf("DNS error")
	}
	var mxRecords []string
	var ttls []uint32
	if r.MsgHdr.AuthenticatedData {
		for _, answer := range r.Answer {
			if mx, ok := answer.(*dns.MX); ok {
				mxRecords = append(mxRecords, mx.Mx)
				ttls = append(ttls, mx.Hdr.Ttl)
			}
		}
	}
	return mxRecords, findMin(ttls), nil
}

type ResultWithTtl struct {
	Result bool
	Ttl    uint32
	Err    error
}

func checkTLSA(mx string) ResultWithTtl {
	tlsaName := fmt.Sprintf("_25._tcp.%s", mx)
	client := &dns.Client{
		Timeout: DNS_TIMEOUT,
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(tlsaName), dns.TypeTLSA)
	m.RecursionDesired = true
	m.SetEdns0(4096, true)
	r, _, err := client.Exchange(m, config.DNS.Address)
	if err != nil {
		return ResultWithTtl{Result: false, Ttl: 0, Err: err}
	}
	if len(r.Answer) == 0 {
		return ResultWithTtl{Result: false, Ttl: 0}
	}
	if r.Rcode != dns.RcodeSuccess && r.Rcode != dns.RcodeNameError {
		return ResultWithTtl{Result: false, Ttl: 0, Err: fmt.Errorf("DNS error")}
	}
	if r.MsgHdr.AuthenticatedData {
		for _, answer := range r.Answer {
			if tlsa, ok := answer.(*dns.TLSA); ok {
				return ResultWithTtl{Result: true, Ttl: tlsa.Hdr.Ttl}
			}
		}
	}
	return ResultWithTtl{Result: false, Ttl: 0}
}

func checkMTASTS(domain string) (string, uint32) {
	hasRecord, err := checkMTASTSRecord(domain)
	if err != nil {
		return "TEMP", 0
	}
	if !hasRecord {
		return "", 0
	}
	mtaSTSURL := "https://mta-sts." + domain + "/.well-known/mta-sts.txt"
	resp, err := http.Get(mtaSTSURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "TEMP", 0
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var policy strings.Builder
	for scanner.Scan() {
		policy.WriteString(scanner.Text() + "\n")
	}
	return generateTlsPolicyMap(policy.String())
}

func checkMTASTSRecord(domain string) (bool, error) {
	client := &dns.Client{
		Timeout: DNS_TIMEOUT,
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("_mta-sts."+domain), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, true)
	r, _, err := client.Exchange(m, config.DNS.Address)
	if err != nil {
		return false, fmt.Errorf("DNS error")
	}
	if r.Rcode != dns.RcodeSuccess && r.Rcode != dns.RcodeNameError {
		return false, fmt.Errorf("DNS error")
	}
	if len(r.Answer) == 0 {
		return false, nil
	}
	for _, answer := range r.Answer {
		if txt, ok := answer.(*dns.TXT); ok {
			for _, txtRecord := range txt.Txt {
				if strings.HasPrefix(txtRecord, "v=STSv1") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func generateTlsPolicyMap(policy string) (string, uint32) {
	lines := strings.Split(strings.TrimSpace(policy), "\n")
	var mxServers []string
	mode := ""
	var maxAge uint32 = 0
	for _, line := range lines {
		if strings.HasPrefix(line, "mode:") {
			mode = strings.TrimSpace(strings.Split(line, ":")[1])
		}
		if strings.HasPrefix(line, "max_age:") {
			age, err := strconv.ParseUint(strings.TrimSpace(strings.Split(line, ":")[1]), 10, 32)
			if err == nil {
				maxAge = uint32(age)
			}
		}
		if strings.HasPrefix(line, "mx:") {
			mxServer := strings.TrimSpace(strings.Split(line, ":")[1])
			if strings.HasPrefix(mxServer, "*.") {
				mxServer = mxServer[1:]
			}
			if !contains(mxServers, mxServer) {
				mxServers = append(mxServers, mxServer)
			}
		}
	}

	if mode == "enforce" {
		mxList := strings.Join(mxServers, ":")
		return fmt.Sprintf("secure match=%s servername=hostname", mxList), maxAge
	}

	return "", maxAge
}

func contains(slice []string, item string) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

func findMin(arr []uint32) uint32 {
	if len(arr) == 0 {
		return 0
	}

	min := arr[0]
	for _, v := range arr {
		if v < min {
			min = v
		}
	}
	return min
}
