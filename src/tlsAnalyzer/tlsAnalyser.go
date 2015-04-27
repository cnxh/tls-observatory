package main

import (
	// stdlib packages

	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"

	// custom packages
	"config"
	"connection"
	"modules/amqpmodule"

	es "modules/elasticsearchmodule"
)

const rxQueue = "conn_scan_results_queue"
const rxRoutKey = "conn_scan_results"
const esIndex = "observer"
const esType = "connection"

var broker *amqpmodule.Broker

//the 2 following structs represent the cipherscan output.

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
		panic(fmt.Sprintf("%s: %s", msg, err))
	}
}

func panicIf(err error) bool {
	if err != nil {
		log.Println(fmt.Sprintf("%s", err))
		return true
	}

	return false
}

// retrieves stored connections ( if any ) for the given scan target
func getConnsforTarget(t string) (map[string]connection.Stored, error) {

	res, err := es.SearchbyTerm(esIndex, esType, "scanTarget.raw", t)

	if err != nil {
		return nil, err
	}

	storedConns := make(map[string]connection.Stored)

	if res.Total > 0 && err != nil {

		for i := 0; i < res.Total; i++ {

			s := connection.Stored{}
			err = json.Unmarshal(*res.Hits[i].Source, &s)

			if err != nil {
				panicIf(err)
				continue
			}

			storedConns[res.Hits[i].Id] = s
		}

		if len(storedConns) > 0 {
			return storedConns, nil
		}
	}

	return storedConns, nil
}

//worker is the main body of the goroutine that handles each received message.
func worker(msgs <-chan []byte) {

	forever := make(chan bool)
	defer wg.Done()

	for d := range msgs {

		info := connection.CipherscanOutput{}

		err := json.Unmarshal(d, &info)

		panicIf(err)

		if err != nil {
			continue
		}

		c, err := info.Stored()

		panicIf(err)
		if err != nil {
			continue
		}

		stored, err := getConnsforTarget(c.ScanTarget)

		if err != nil {
			panicIf(err)
			continue
		}

		err = updateAndPushConnections(c, stored)

		panicIf(err) //Should we requeue the connection in case of error?
	}

	<-forever
}

func updateAndPushConnections(newconn connection.Stored, conns map[string]connection.Stored) error {

	err := error(nil)

	if len(conns) > 0 {
		for id, conn := range conns {
			if conn.ObsoletedBy == "" {
				if newconn.Equal(conn) {

					log.Println("Updating doc for ", conn.ScanTarget)
					conn.LastSeenTimestamp = newconn.LastSeenTimestamp

					jsonConn, err := json.Marshal(conn)

					if err == nil {
						_, err = es.Push(esIndex, esType, "", jsonConn)
					}

					break

				} else {

					log.Println("Pushing new doc for ", conn.ScanTarget)

					jsonConn, err := json.Marshal(newconn)

					obsID := ""

					if err != nil {
						break
					}

					obsID, err = es.Push(esIndex, esType, "", jsonConn)

					if err != nil {
						break
					}

					conn.ObsoletedBy = obsID

					jsonConn, err = json.Marshal(conn)

					obsID, err = es.Push(esIndex, esType, id, jsonConn)
				}
			}
		}
	} else {

		log.Println("No older doc found for ", newconn.ScanTarget)

		jsonConn, err := json.Marshal(newconn)

		if err == nil {
			_, err = es.Push(esIndex, esType, "", jsonConn)
		}

	}

	return err
}

func printIntro() {
	fmt.Println(`
	##################################
	#         TLSAnalyzer            #
	##################################
	`)
}

var wg sync.WaitGroup

func main() {
	var (
		err error
	)

	printIntro()

	conf := config.AnalyzerConfig{}

	var cfgFile string
	flag.StringVar(&cfgFile, "c", "/etc/observer/analyzer.cfg", "Input file csv format")
	flag.Parse()

	_, err = os.Stat(cfgFile)
	failOnError(err, "Missing configuration file from '-c' or /etc/observer/retriever.cfg")

	conf, err = config.AnalyzerConfigLoad(cfgFile)
	if err != nil {
		conf = config.GetAnalyzerDefaults()
	}

	cores := runtime.NumCPU()
	runtime.GOMAXPROCS(cores * conf.General.GoRoutines)

	err = es.RegisterConnection(conf.General.ElasticSearch)

	failOnError(err, "Failed to register ElasticSearch")

	broker, err = amqpmodule.RegisterURL(conf.General.RabbitMQRelay)

	failOnError(err, "Failed to register RabbitMQ")

	msgs, err := broker.Consume(rxQueue, rxRoutKey)

	if err != nil {
		failOnError(err, "Failed to Consume from receiving queue")
	}

	for i := 0; i < cores; i++ {
		wg.Add(1)
		go worker(msgs)
	}

	wg.Wait()
}
