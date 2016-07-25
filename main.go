package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
)

// urlsInProgress is a wait group, for concurrency
var urlsInProgress sync.WaitGroup

// RedirectError is a custom error type for following redirects, and can be safely ignored
type RedirectError struct {
	RedirectRequest *http.Request
}

// Error method returns a string of the error
func (err *RedirectError) Error() string {
	return fmt.Sprintf("Redirect not followed to: %v", err.RedirectRequest.URL.String())
}

// Request body content
var body string

// Target URL value
var targetURLs []*url.URL

// Cookie jar value
var jar *cookiejar.Jar

// Request type (default POST)
var requestMethod = "POST"

// Follow redirects (default false)
var followRedirects = false

// Number of requests (default 100)
var requestsCount = 100

// Verbose logging (default false)
var verbose = false

// Function main is the entrypoint to the application. It sends the work to the appropriate functions, sequentially.
func main() {
	// Change output location of logs
	log.SetOutput(os.Stdout)

	// Set the usage string
	setUsage()

	// Check the config file
	err := checkConfig()
	if err != nil {
		log.Println(err.Error())
		flag.Usage()
		os.Exit(1)
	}

	// Send the requests concurrently
	log.Println("Requests begin.")
	responses, errors := sendRequests()
	if len(errors) != 0 {
		for err := range errors {
			log.Printf("[ERROR] %s\n", err.Error())
		}
	}

	// Make sure all response bodies are closed- memory leaks otherwise
	defer func() {
		for resp := range responses {
			resp.Body.Close()
		}
	}()

	// Compare the responses for uniqueness
	uniqueResponses, errors := compareResponses(responses)
	if len(errors) != 0 {
		for err := range errors {
			log.Printf("[ERROR] %s\n", err.Error())
		}
	}

	// Make sure all response bodies are closed- memory leaks otherwise
	defer func() {
		for resp := range uniqueResponses {
			resp.Body.Close()
		}
	}()

	// Output the responses
	outputResponses(uniqueResponses)

	// Echo completion
	log.Println("Complete.")
}

// Function setUsage overrides the flag package's usage output to something
// more specific to this package
func setUsage() {
	flag.Usage = func() {
		// TODO: Add in usage output
	}
}

// Function checkConfig checks that all necessary configuration fields are given
// in a valid config file, and parses it for data.
// Returns a custom error if something went wrong.
func checkConfig() error {
	// Viper initialization
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	viper.SetConfigType("toml")
	err := viper.ReadInConfig()
	if err != nil {
		return fmt.Errorf("[ERROR] Error reading config file: %s", err.Error())
	}

	// Check for required values
	if !viper.IsSet("request.method") || !viper.IsSet("request.targets") {
		return fmt.Errorf("[ERROR] Method and target(s) must be set.")
	}

	// Get targets
	requestMethod = viper.GetString("request.method")
	for _, target := range viper.GetStringSlice("request.targets") {
		targetURL, err := url.Parse(target)
		if err != nil {
			return fmt.Errorf("[ERROR] Invalid URL provided in targets configuration: %s", target)
		}
		targetURLs = append(targetURLs, targetURL)
	}

	// Get request method
	if method := strings.ToUpper(viper.GetString("request.method")); method == "POST" || method == "GET" || method == "PUT" || method == "HEAD" {
		requestMethod = method
	} else {
		// Invalid request type specified
		return fmt.Errorf("[ERROR] Invalid request method specified: %s", method)
	}

	// Get the request body, if set
	if viper.IsSet("request.body") {
		body = viper.GetString("request.body")
	}

	// Initialize the cookie jar
	jar, _ = cookiejar.New(nil)
	var cookies []*http.Cookie
	// Get the cookies to pass to the request
	if viper.IsSet("request.cookies") {
		for _, c := range viper.GetStringSlice("request.cookies") {
			// Split the cookie name and value
			vals := strings.Split(c, "=")
			cookieName := strings.TrimSpace(vals[0])
			cookieValue := strings.TrimSpace(vals[1])

			// Create the cookie
			cookie := &http.Cookie{
				Name:  cookieName,
				Value: cookieValue,
			}

			// Add the cookie to the existing slice of cookies
			cookies = append(cookies, cookie)
		}

		// NOTE: Right now, it is assumed that the URLs are on the same domain. If there are use cases in the future where URLs on separate domains need to be used, change this.
		// Set the cookies to the appropriate URL
		jar.SetCookies(targetURLs[0], cookies)
	}

	// Follow redirects
	if viper.IsSet("request.redirects") {
		followRedirects = viper.GetBool("request.redirects")
	}

	// Request count
	if viper.IsSet("request.count") {
		requestsCount = viper.GetInt("request.count")
	}

	// Verbose logging
	if viper.IsSet("request.verbose") {
		verbose = viper.GetBool("request.verbose")
	}

	return nil
}

// Function sendRequests takes care of sending the requests to the target concurrently.
// Errors are passed back in a channel of errors. If the length is zero, there were no errors.
func sendRequests() (responses chan *http.Response, errors chan error) {
	// Initialize the concurrency objects
	responses = make(chan *http.Response, requestsCount)
	errors = make(chan error, requestsCount)
	urlsInProgress.Add(requestsCount)

	// TODO: Send requests to multiple URLs (if present) the same number of times
	// RESUME

	// VERBOSE
	if verbose {
		log.Printf("[VERBOSE] Sending %d %s requests to %s\n", requestsCount, requestMethod, targetURL.String())
		if body != "" {
			log.Printf("[VERBOSE] Request body: %s", body)
		}
	}
	for i := 0; i < numRequests; i++ {
		go func(index int) {
			// Ensure that the waitgroup element is returned
			defer urlsInProgress.Done()

			// Convert the request body to an io.Reader interface, to pass to the request.
			// This must be done in the loop, because any call to client.Do() will
			// read the body contents on the first time, but not any subsequent requests.
			requestBody := strings.NewReader(body)

			// Declare HTTP request method and URL
			req, err := http.NewRequest(requestMethod, targetURL.String(), requestBody)
			if err != nil {
				errors <- fmt.Errorf("Error in forming request: %v", err.Error())
				return
			}

			// Create the HTTP client
			// Using Cookie jar
			// Ignoring TLS errors
			// Ignoring redirects (more accurate output), depending on user flag
			// Implementing a connection timeouts, for slow clients & servers (especially important with race conditions on the server)
			var client http.Client
			if followRedirects {
				client = http.Client{
					Jar: jar,
					Transport: &http.Transport{
						TLSClientConfig: &tls.Config{
							InsecureSkipVerify: true,
						},
					},
					Timeout: 20 * time.Second,
				}
			} else {
				client = http.Client{
					Jar: jar,
					Transport: &http.Transport{
						TLSClientConfig: &tls.Config{
							InsecureSkipVerify: true,
						},
					},
					CheckRedirect: func(req *http.Request, via []*http.Request) error {
						// Craft the custom error
						redirectError := RedirectError{req}
						return &redirectError
					},
					Timeout: 20 * time.Second,
				}
			}

			// Make the request
			resp, err := client.Do(req)
			// Check the error type from the request
			if err != nil {
				if uErr, ok := err.(*url.Error); ok {
					if rErr, ok2 := uErr.Err.(*RedirectError); ok2 {
						// Redirect error
						// VERBOSE
						if *flagVerbose {
							log.Printf("[VERBOSE] %v\n", rErr)
						}
						// Add the response to the responses channel, because it is still valid
						responses <- resp
					} else {
						// URL Error, but not a redirect error
						errors <- fmt.Errorf("Error in request #%v: %v\n", index, err)
					}
				} else {
					// Other type of error
					errors <- fmt.Errorf("Error in request #%v: %v\n", index, err)
				}
			} else {
				// Add the response to the responses channel
				responses <- resp
			}
		}(i)
	}

	// Wait for the URLs to finish sending
	urlsInProgress.Wait()

	// VERBOSE
	if *flagVerbose {
		log.Printf("[VERBOSE] Requests complete.")
	}

	// Close the response and error chanels, so they don't block on the range read
	close(responses)
	close(errors)

	return
}

// Function compareResponses compares the responses returned from the requests,
// and adds them to a map, where the key is an *http.Response, and the value is
// the number of similar responses observed.
func compareResponses(responses chan *http.Response) (uniqueResponses map[*http.Response]int, errors chan error) {
	// Initialize the unique responses map
	uniqueResponses = make(map[*http.Response]int)

	// Initialize the error channel
	errors = make(chan error, len(responses))

	// VERBOSE
	if *flagVerbose {
		log.Printf("[VERBOSE] Unique response comparison begin.\n")
	}

	// Compare the responses, one at a time
	for resp := range responses {
		// Read the response body
		respBody, err := readResponseBody(resp)
		if err != nil {
			errors <- fmt.Errorf("Error reading response body: %s", err.Error())

			// Exit this loop
			continue
		}

		// Add an entry, if the unique responses map is empty
		if len(uniqueResponses) == 0 {
			uniqueResponses[resp] = 0
		} else {
			// Add to the unique responses map, if no similar ones exist
			for uResp := range uniqueResponses {
				// Read the unique response body
				uRespBody, err := readResponseBody(uResp)
				if err != nil {
					errors <- fmt.Errorf("Error reading unique response body: %s", err.Error())

					// Exit the inner loop
					continue
				}

				// Compare the response bodies
				respBodyMatch := false
				if string(respBody) == string(uRespBody) {
					respBodyMatch = true
				}

				// Compare response status code, body content, and content length
				if resp.StatusCode == uResp.StatusCode && resp.ContentLength == uResp.ContentLength && respBodyMatch {
					// Similar, increase count
					uniqueResponses[uResp]++
					// Exit inner loop
					continue
				} else {
					// Unique, add to unique responses
					uniqueResponses[resp] = 0
					// Exit inner loop
					continue
				}
			}
		}
	}

	// VERBOSE
	if *flagVerbose {
		log.Printf("[VERBOSE] Unique response comparision complete.\n")
	}

	// Close the error channel
	close(errors)

	return
}

func outputResponses(uniqueResponses map[*http.Response]int) {
	// Display the responses
	log.Printf("Responses:\n")
	for resp, count := range uniqueResponses {
		fmt.Printf("Response:\n")
		fmt.Printf("[Status Code] %v\n", resp.StatusCode)
		fmt.Printf("[Protocol] %v\n", resp.Proto)
		if len(resp.Header) != 0 {
			fmt.Println("[Headers]")
			for header, value := range resp.Header {
				fmt.Printf("\t%v: %v\n", header, value)
			}
		}
		location, err := resp.Location()
		if err != http.ErrNoLocation {
			fmt.Printf("[Location] %v\n", location.String())
		}
		respBody, err := readResponseBody(resp)
		if err != nil {
			fmt.Println("[Body] ")
			fmt.Printf("[ERROR] Error reading body: %v.", err)
		} else {
			fmt.Printf("[Body]\n%s\n", respBody)
			// Close the response body
			resp.Body.Close()
		}
		fmt.Printf("Similar: %v\n\n", count)
	}
}

// Function readResponseBody is a helper function to read the content form a response's body,
// and refill the body with another io.ReadCloser, so that it can be read again.
func readResponseBody(resp *http.Response) (content []byte, err error) {
	// Get the content
	content, err = ioutil.ReadAll(resp.Body)

	// Reset the response body
	rCloser := ioutil.NopCloser(bytes.NewBuffer(content))
	resp.Body = rCloser

	return
}

// TODO: Add option to send a second request body or URL at the same time, the same number of times (useful for adding 2 values to a database)
// TODO: Add option to include multiple session cookie values. Cookies for each request will be semicolon-delimited, and newline characters will delimit cookies for different requests.
