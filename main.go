package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/url"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/ryanuber/go-glob"
	"github.com/schollz/progressbar/v3"
	flag "github.com/spf13/pflag"
)

type Config struct {
	// The number of concurrent workers to test with
	Workers int

	// The HTTP methods to test
	Methods []string

	// The delay which signifies a timeout between the frontend and backend servers
	Delay time.Duration

	// The Transfer-Encoding headers to test
	Mutations map[string]string

	// The maximum number of desyncs to find in a target
	StopAfter uint

	// Whether to show the progress bar
	ShowProgress bool

	// Whether to user verbose or debugging output
	Verbose bool
	Debug   bool

	// The filenames to save to
	OutFilename  string
	BaseFilename string
	ErrFilename  string
}

func main() {
	conf := Config{}

	// Scanning options
	flag.IntVarP(&conf.Workers, "workers", "c", 10, "the number of concurrent workers")
	flag.StringSliceVarP(&conf.Methods, "methods", "m", []string{"GET", "POST", "PUT", "DELETE"}, "the methods to test")
	flag.DurationVarP(&conf.Delay, "delay", "", 5*time.Second, "the extra time delay on top of the base time that indicates the service is vulnerable")
	enabled := flag.StringSliceP("enable", "e", nil, "globs of modules to enable")
	disabled := flag.StringSliceP("disable", "d", nil, "globs of modules to disable")
	flag.UintVarP(&conf.StopAfter, "stop-after", "x", 0, "the number of smuggling vulnerabilities to find in a host before stopping testing on it. This won't cancel already queued tests, so slightly more than this number of vulnerabilities may be found")

	// Output display options
	flag.BoolVarP(&conf.ShowProgress, "progress", "p", false, "show a progress bar instead of output discovered vulnerabilities to stdout")
	flag.BoolVarP(&conf.Verbose, "verbose", "v", false, "print scanned hosts to stdout")
	flag.BoolVarP(&conf.Debug, "debug", "", false, "time each request and output the times to stdout")

	// Output file options
	flag.StringVarP(&conf.OutFilename, "output", "o", "", "the log file to write to")
	flag.StringVarP(&conf.BaseFilename, "base", "b", "", "the base file with request times to use (default \"smuggles.base\")")
	flag.StringVarP(&conf.ErrFilename, "error-log", "", "", "the file to log errors to")
	outDir := flag.StringP("dir", "O", "", "the directory to output the log, error log, and base file to")

	// Early exit flags
	generatePoc := flag.BoolP("poc", "", false, "generate a PoC from a provided line of the log file of format <method> <url> <desync type> <mutation name> and exit")
	scriptFile := flag.StringP("script", "", "", "generate a Turbo Intruder script using the specified file as a base, to verify the smuggling issue with a 404 request from a provided line of the log file of format <method> <url> <desync type> <mutation name>")
	gadget := flag.StringP("mutation", "", "", "print the specified Transfer-Encoding header mutation and exit")
	list := flag.BoolP("list", "l", false, "list the enabled mutation names and exit")

	flag.Parse()

	// Generate the enabled mutations
	all := generateMutations()
	conf.Mutations = make(map[string]string, 0)
	for m := range all {
		include := true

		if enabled != nil && len(*enabled) > 0 {
			include = false
			for _, e := range *enabled {
				if glob.Glob(e, m) {
					include = true
					break
				}
			}
		}

		if disabled != nil && len(*disabled) > 0 {
			for _, d := range *disabled {
				if glob.Glob(d, m) {
					include = false
					break
				}
			}
		}

		if include {
			conf.Mutations[m] = all[m]
		}
	}

	// Check for options that lead to early exit
	if *list {
		keys := make([]string, len(conf.Mutations))
		i := 0
		for k := range conf.Mutations {
			keys[i] = k
			i++
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Println(k)
		}
		os.Exit(0)
	}

	if *gadget != "" {
		header, ok := conf.Mutations[*gadget]
		if ok {
			fmt.Println(header)
			os.Exit(0)
		} else {
			fmt.Println("Mutation not found")
			os.Exit(1)
		}
	}

	if *generatePoc {
		if flag.NArg() != 4 {
			fmt.Println("Positional arguments should be: <method> <url> <desync type> <mutation name>")
			fmt.Println("e.g.: smuggles --poc GET https://example.com CL.TE lineprefix-space")
			os.Exit(1)
		}

		poc, err := generatePoC(conf, flag.Arg(0), flag.Arg(1), flag.Arg(2), flag.Arg(3))
		if err != nil {
			fmt.Printf("Couldn't generate PoC: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s", string(poc))
		os.Exit(0)
	}

	if *scriptFile != "" {
		if flag.NArg() != 4 {
			fmt.Println("Positional arguments should be: <method> <url> <desync type> <mutation name>")
			fmt.Println("e.g.: smuggles --script resources/clte.py GET https://example.com CL.TE lineprefix-space")
			os.Exit(1)
		}

		script, err := generateScript(conf, *scriptFile, flag.Arg(0), flag.Arg(1), flag.Arg(3))
		if err != nil {
			fmt.Printf("Error generating script: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("%s", string(script))
		os.Exit(0)
	}

	urls := make([]*url.URL, 0)

	// Logging
	var reslog *log.Logger
	var errlog *log.Logger
	if *outDir != "" {
		if conf.OutFilename == "" {
			conf.OutFilename = path.Join(*outDir, "smuggles.log")
		}
		if conf.BaseFilename == "" {
			conf.BaseFilename = path.Join(*outDir, "smuggles.base")
		}
		if conf.ErrFilename == "" {
			conf.ErrFilename = path.Join(*outDir, "smuggles.errors")
		}
	}

	if conf.OutFilename != "" {
		f, err := os.OpenFile(conf.OutFilename, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			fmt.Printf("Failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		outputs := []io.Writer{f}
		if !conf.ShowProgress {
			outputs = append(outputs, os.Stdout)
		}
		mw := io.MultiWriter(outputs...)
		reslog = log.New(mw, "", 0)
	} else if conf.ShowProgress {
		fmt.Println("WARNING: progress bar being shown and no output file specified - discovered vulnerabilities will not be outputted anywhere!")
		reslog = log.New(ioutil.Discard, "", 0)
	} else {
		reslog = log.New(os.Stdout, "", 0)
	}

	if conf.ErrFilename != "" {
		f, err := os.OpenFile(conf.ErrFilename, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			fmt.Printf("Failed to open error log file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		outputs := []io.Writer{f}
		if !conf.ShowProgress {
			outputs = append(outputs, os.Stdout)
		}

		mw := io.MultiWriter(outputs...)
		errlog = log.New(mw, "ERROR:", 0)
	} else {
		errlog = log.New(os.Stderr, "ERROR:", 0)
	}

	// The base times for standard requests
	var base map[string]time.Duration
	if conf.BaseFilename == "" {
		conf.BaseFilename = "smuggles.base"
	}
	baseFile, err := os.OpenFile(conf.BaseFilename, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		fmt.Printf("Failed to open base file: %v\n", err)
		os.Exit(1)
	}
	defer baseFile.Close()
	jsonBytes, err := ioutil.ReadAll(baseFile)
	if err != nil {
		fmt.Printf("Failed to read base file: %v\n", err)
		os.Exit(1)
	}

	if len(jsonBytes) > 0 {
		err = json.Unmarshal(jsonBytes, &base)
		if err != nil {
			fmt.Printf("Failed to parse base file as JSON: %v\n", err)
			os.Exit(1)
		}
	} else {
		base = make(map[string]time.Duration, 0)
	}

	// Genrate the workers
	workers := make([]Worker, conf.Workers)
	errs := make(chan error)
	for i := range workers {
		workers[i] = Worker{Conf: conf, Errs: errs}
	}

	// Fill in any missing entries in the base file
	fmt.Println("Getting missing base times...")
	baseUrls := make(chan *url.URL)
	baseResults := make(chan BaseResult)
	baseWg := sync.WaitGroup{}
	baseWg.Add(conf.Workers)
	baseMux := sync.RWMutex{}
	for i := range workers {
		go workers[i].BaseTimes(baseUrls, baseResults, baseWg.Done)
	}

	// Read from stdin
	go func() {
		var bar *progressbar.ProgressBar
		if conf.ShowProgress {
			bar = progressbar.Default(-1)
		}
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			urlStr := scanner.Text()
			u, err := url.Parse(urlStr)
			if err != nil {
				errlog.Println(err)
			}
			baseMux.RLock()
			_, exists := base[u.String()]
			baseMux.RUnlock()
			if !exists {
				baseUrls <- u
				if conf.ShowProgress {
					bar.Add(1)
				}
			}
			urls = append(urls, u)
		}
		close(baseUrls)
	}()

	// Wait for workers to all be done
	go func() {
		baseWg.Wait()
		close(baseResults)
	}()

	// Handle errors
	go func() {
		for err := range errs {
			errlog.Println(err)
		}
	}()

	for r := range baseResults {
		baseMux.Lock()
		base[r.Url.String()] = r.Time
		baseMux.Unlock()
		if conf.Verbose {
			fmt.Printf("%s %d\n", r.Url, r.Time)
		}
	}

	// Save the file
	b, err := json.Marshal(base)
	if err != nil {
		errlog.Printf("Error marshalling base times to JSON: %v\n", err)
		return
	}

	_, err = baseFile.Seek(0, 0)
	if err != nil {
		errlog.Printf("Error seeking to start of file: %v\n", err)
	}

	_, err = baseFile.Write(b)
	if err != nil {
		errlog.Printf("Error writing base to file: %v\n", err)
	}
	baseFile.Close()

	// Now smuggle test
	fmt.Println("Testing smuggling...")

	// Counts the number of issues found on each host for use with the -x flag
	vulns := make(map[string]uint, 0)
	vulnsMux := sync.RWMutex{}

	// Generate a slice of all the tests to choose from at random
	tests := make([]SmuggleTest, 0)
	for _, u := range urls {
		// We only want to run the tests if we have a base time for this URL
		if _, ok := base[u.String()]; !ok {
			continue
		}

		for m := range conf.Mutations {
			for _, v := range conf.Methods {
				timeout := base[u.String()] + conf.Delay
				t := SmuggleTest{
					Url:      u,
					Method:   v,
					Mutation: m,
					Status:   SAFE,
					Timeout:  timeout,
				}
				tests = append(tests, t)
			}
		}
	}

	// Start the workers
	testsChan := make(chan SmuggleTest)
	testResults := make(chan SmuggleTest)
	testsWg := sync.WaitGroup{}
	testsWg.Add(conf.Workers)
	for i := range workers {
		go workers[i].SmuggleTest(testsChan, testResults, testsWg.Done)
	}

	// Send tests
	go func() {
		var bar *progressbar.ProgressBar
		if conf.ShowProgress {
			bar = progressbar.Default(int64(len(tests)))
		}

		rand.Seed(time.Now().Unix())
		for len(tests) > 0 {
			i := rand.Intn(len(tests))
			t := tests[i]
			tests = append(tests[:i], tests[i+1:]...)
			send := true
			if conf.StopAfter > 0 {
				vulnsMux.RLock()
				send = vulns[t.Url.String()] < conf.StopAfter
				vulnsMux.RUnlock()

			}
			if send {
				testsChan <- t
			}
			if conf.ShowProgress {
				bar.Add(1)
			}
			if conf.Verbose {
				fmt.Printf("Testing: %s %s %s\n", t.Method, t.Url, t.Mutation)
			}
		}
		close(testsChan)
	}()

	// Wait for workers to be done
	go func() {
		testsWg.Wait()
		close(testResults)
	}()

	for t := range testResults {
		if t.Status != SAFE {
			reslog.Printf("%s %s %s %s\n", t.Method, t.Url, t.Status, t.Mutation)
			if conf.StopAfter > 1 {
				vulnsMux.Lock()
				vulns[t.Url.String()] += 1
				vulnsMux.Unlock()
			}
		}
	}
}
