/*
	A FastAGI parallel benchmark in go

	Copyright (C) 2013 - 2014, Lefteris Zafiris <zaf.000@gmail.com>

	This program is free software, distributed under the terms of
	the GNU General Public License Version 3. See the LICENSE file
	at the top of the source tree.
*/

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	shutdown int32
	file     *os.File
	writer   *bufio.Writer
	conf     = flag.String("conf", "", "Configuration file")
	single   = flag.Bool("single", false, "Connect and run only once")
	debug    = flag.Bool("debug", false, "Write detailed statistics output to csv file")
	host     = flag.String("host", "127.0.0.1", "FAstAGI server host")
	port     = flag.String("port", "4573", "FastAGI server port")
	runs     = flag.Float64("runs", 1, "Number of runs per second")
	sess     = flag.Int("sess", 1, "Sessions per run")
	delay    = flag.Int("delay", 50, "Delay in AGI responses to the server (milliseconds)")
	req      = flag.String("req", "myagi?file=echo-test", "AGI request")
	arg      = flag.String("arg", "", "Argument to pass to the FastAGI server")
	cid      = flag.String("cid", "Unknown", "Caller ID")
	ext      = flag.String("ext", "100", "Called extension")
)

// Benchmark session data
type Bench struct {
	Count      int64
	Active     int64
	Fail       int64
	AvrDur     int64
	LogChan    chan string
	TimeChan   chan int64
	RunDelay   time.Duration
	ReplyDelay time.Duration
	Env        []byte
	Payload    []AgiMsg
}

// AGI Playload data
type AgiMsg struct {
	Msg   string
	Delay int
}

// Configuration data
type Config struct {
	AgiEnv     []string
	AgiPayload []AgiMsg
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()
	if *debug {
		// Open log file for writing
		file, err := os.Create("bench-" + strconv.FormatInt(time.Now().Unix(), 10) + ".csv")
		if err != nil {
			log.Fatalln("Failed to create file:", err)
		}
		writer = bufio.NewWriter(file)
		defer func() {
			writer.Flush()
			file.Close()
		}()
		writer.WriteString("#Started benchmark at: " + time.Now().String() + "\n")
		writer.WriteString(fmt.Sprintf("#Host: %v\n#Port: %v\n#Runs: %v\n", *host, *port, *runs))
		writer.WriteString(fmt.Sprintf("#Sessions: %v\n#Delay: %v\n#Reguest: %v\n", *sess, *delay, *req))
		writer.WriteString("#completed,active,duration\n")
	}
	wgMain := new(sync.WaitGroup)
	wgMain.Add(1)
	b := benchInit()
	if *single {
		// Run once
		go agiConnection(b, wgMain)
	} else {
		// Start benchmark and wait for users input to stop
		go agiBench(b, wgMain)
		bufio.NewReader(os.Stdin).ReadString('\n')
		atomic.StoreInt32(&shutdown, 1)
	}
	wgMain.Wait()
	if *debug {
		writer.WriteString("#Stopped benchmark at: " + time.Now().String() + "\n")
	}
}

// Initialize benchmark session
func benchInit() *Bench {
	var confData Config
	b := new(Bench)
	b.LogChan = make(chan string, *sess*2)
	b.TimeChan = make(chan int64, *sess*2)
	b.RunDelay = time.Duration(1e9 / *runs) * time.Nanosecond
	b.ReplyDelay = time.Duration(*delay) * time.Millisecond
	if *conf != "" {
		// Parse config file
		file, err := os.Open(*conf)
		if err != nil {
			log.Println("Failed to open config file, using default settings:", err)
		} else {
			decoder := json.NewDecoder(file)
			err = decoder.Decode(&confData)
			if err != nil {
				log.Println("Failed to parse config file, using default settings:", err)
			}
		}
	}
	b.Env = agiInit(confData.AgiEnv)
	b.Payload = confData.AgiPayload
	return b
}

// Run benchmark and gather data
func agiBench(b *Bench, wg *sync.WaitGroup) {
	defer wg.Done()
	wgBench := new(sync.WaitGroup)
	wgBench.Add(*sess)
	// Spawn pool of paraller runs
	for i := 0; i < *sess; i++ {
		ticker := time.Tick(b.RunDelay)
		go func() {
			defer wgBench.Done()
			wgConn := new(sync.WaitGroup)
			for atomic.LoadInt32(&shutdown) == 0 {
				<-ticker
				wgConn.Add(1)
				go agiConnection(b, wgConn)
			}
			wgConn.Wait()
		}()
	}
	wgMon := new(sync.WaitGroup)
	// Write to log file
	if *debug {
		wgMon.Add(1)
		go logger(b, wgMon)
	}
	// Calculate session duration and display some output on the console
	wgMon.Add(2)
	go calcAvrg(b, wgMon)
	go consoleOutput(b, wgMon)
	// Wait all FastAGI sessions to end
	wgBench.Wait()
	close(b.LogChan)
	close(b.TimeChan)
	// Wait logger and console output to finish
	wgMon.Wait()
}

// Connect to the AGI server and send AGI payload
func agiConnection(b *Bench, wg *sync.WaitGroup) {
	defer wg.Done()
	start := time.Now()
	conn, err := net.Dial("tcp", net.JoinHostPort(*host, *port))
	if err != nil {
		atomic.AddInt64(&b.Fail, 1)
		if *debug {
			b.LogChan <- fmt.Sprintf("# %s\n", err)
		}
		return
	}
	atomic.AddInt64(&b.Active, 1)
	scanner := bufio.NewScanner(conn)
	// Send AGI initialisation data
	conn.Write(b.Env)
	if b.Payload == nil {
		// Reply with '200' to all messages from the AGI server until it hangs up
		for scanner.Scan() {
			if scanner.Text() == "HANGUP" {
				conn.Write([]byte("200 result=1\nHANGUP\n"))
				break
			}
			time.Sleep(b.ReplyDelay)
			conn.Write([]byte("200 result=0\n"))
		}
	} else {
		// Use the AGI Payload from loaded config file
		for _, pld := range b.Payload {
			if !scanner.Scan() {
				break
			}
			if scanner.Text() == "HANGUP" {
				conn.Write([]byte("200 result=1\nHANGUP\n"))
				break
			}
			time.Sleep(time.Duration(pld.Delay) * time.Millisecond)
			conn.Write([]byte(pld.Msg + "\n"))
		}
	}
	conn.Close()
	elapsed := time.Since(start)
	b.TimeChan <- elapsed.Nanoseconds()
	atomic.AddInt64(&b.Active, -1)
	atomic.AddInt64(&b.Count, 1)
	if *debug {
		b.LogChan <- fmt.Sprintf("%d,%d,%d\n", atomic.LoadInt64(&b.Count),
			atomic.LoadInt64(&b.Active), elapsed.Nanoseconds())
	}
}

// Write to log file
func logger(b *Bench, wg *sync.WaitGroup) {
	defer wg.Done()
	for logMsg := range b.LogChan {
		writer.WriteString(logMsg)
	}
}

// Calculate Average session duration for the last 1000 sessions
func calcAvrg(b *Bench, wg *sync.WaitGroup) {
	defer wg.Done()
	var sessions int64
	for dur := range b.TimeChan {
		sessions++
		atomic.StoreInt64(&b.AvrDur, (atomic.LoadInt64(&b.AvrDur)*(sessions-1)+dur)/sessions)
		if sessions >= 10000 {
			sessions = 0
		}
	}
}

// Display pretty output to the user
func consoleOutput(b *Bench, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		fmt.Print("\033[2J\033[H") //Clear screen
		fmt.Println("Running paraller AGI bench to:", net.JoinHostPort(*host, *port),
			"\nPress Enter to stop.\n",
			"\nA new run each:", b.RunDelay,
			"\nSessions per run:", *sess,
			"\nReply delay:", b.ReplyDelay,
			"\n\nFastAGI Sessions\nActive:", atomic.LoadInt64(&b.Active),
			"\nCompleted:", atomic.LoadInt64(&b.Count),
			"\nDuration:", atomic.LoadInt64(&b.AvrDur), "ns (last 10000 sessions average)",
			"\nFailed:", atomic.LoadInt64(&b.Fail))
		if atomic.LoadInt32(&shutdown) != 0 {
			fmt.Println("Stopping...")
			if atomic.LoadInt64(&b.Active) == 0 {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
}

// Generate AGI Environment data
func agiInit(env []string) []byte {
	var envData string
	if env != nil {
		// Env from config file
		for _, par := range env {
			envData += fmt.Sprintln(par)
		}
	} else {
		// Default Env data with cli parameters
		envData = fmt.Sprintln("agi_network: yes")
		if len(*req) > 0 {
			envData += fmt.Sprintln("agi_network_script: " + *req)
			envData += fmt.Sprintln("agi_request: agi://" + *host + "/" + *req)
		} else {
			envData += fmt.Sprintln("agi_request: agi://" + *host)
		}
		envData += fmt.Sprintln("agi_channel: SIP/1234-00000000")
		envData += fmt.Sprintln("agi_language: en")
		envData += fmt.Sprintln("agi_type: SIP")
		envData += fmt.Sprintln("agi_uniqueid: 1410638774.0")
		envData += fmt.Sprintln("agi_version: 10.1.1.0")
		envData += fmt.Sprintln("agi_callerid: " + *cid)
		envData += fmt.Sprintln("agi_calleridname: " + *cid)
		envData += fmt.Sprintln("agi_callingpres: 67")
		envData += fmt.Sprintln("agi_callingani2: 0")
		envData += fmt.Sprintln("agi_callington: 0")
		envData += fmt.Sprintln("agi_callingtns: 0")
		envData += fmt.Sprintln("agi_dnid: " + *ext)
		envData += fmt.Sprintln("agi_rdnis: unknown")
		envData += fmt.Sprintln("agi_context: default")
		envData += fmt.Sprintln("agi_extension: " + *ext)
		envData += fmt.Sprintln("agi_priority: 1")
		envData += fmt.Sprintln("agi_enhanced: 0.0")
		envData += fmt.Sprintln("agi_accountcode: ")
		envData += fmt.Sprintln("agi_threadid: -1281018784")
		if len(*arg) > 0 {
			envData += fmt.Sprintln("agi_arg_1: " + *arg)
		}
	}
	return []byte(envData + "\n")
}
