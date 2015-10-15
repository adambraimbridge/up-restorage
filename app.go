package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"sync"
)

func main() {

	f, err := os.Create("/tmp/cpuprof")
	if err != nil {
		panic(err)
	}
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()

	index := NewElasticEngine()

	ah := apiHandlers{index}

	m := mux.NewRouter()
	http.Handle("/", handlers.CombinedLoggingHandler(os.Stdout, m))

	m.HandleFunc("/{collection}/search", ah.searchHandler).Methods("GET")
	m.HandleFunc("/{collection}/count", ah.countHandler).Methods("GET")
	m.HandleFunc("/{collection}/{id}", ah.idReadHandler).Methods("GET")
	m.HandleFunc("/{collection}/{id}", ah.idWriteHandler).Methods("PUT")
	m.HandleFunc("/{collection}/", ah.dropHandler).Methods("DELETE")
	m.HandleFunc("/{collection}/", ah.putAllHandler).Methods("PUT")
	m.HandleFunc("/{collection}/", ah.dumpAll).Methods("GET")

	go func() {
		fmt.Printf("listening on 8082..")
		err = http.ListenAndServe(":8082", nil)
		if err != nil {
			log.Printf("web stuff failed: %v\n", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// wait for ctrl-c
	<-c
	println("exiting")
	index.Close()

	f, err = os.Create("/tmp/memprof")
	if err != nil {
		panic(err)
	}

	pprof.WriteHeapProfile(f)
	f.Close()

	return
}

type apiHandlers struct {
	index Engine
}

func (ah *apiHandlers) idReadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	collection := vars["collection"]

	found, art, err := ah.index.Load(collection, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("document with id %s was not found\n", id)))
		return
	}
	w.Header().Add("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(art)
}

func (ah *apiHandlers) putAllHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	collection := vars["collection"]

	errCh := make(chan error, 2)
	docCh := make(chan Document)

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(docCh)

		dec := json.NewDecoder(r.Body) //TODO: bufio?
		for {
			var doc Document
			err := dec.Decode(&doc)
			if err == io.EOF {
				return
			}
			if err != nil {
				errCh <- err
				log.Printf("failed to decode json. aborting: %v\n", err.Error())
				return
			}
			docCh <- doc
		}

	}()

	for x := 0; x < 8; x++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for doc := range docCh {
				err := ah.index.Write(collection, getId(doc), doc)
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()

	select {
	case err := <-errCh:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	default:
		println("returning normally")
		return
	}

}

func getId(doc Document) string {
	// TODO: obviously this should be parameterised
	if id, ok := doc["uuid"].(string); ok {
		return id
	}
	if id, ok := doc["id"].(string); ok {
		return id
	}
	panic("no id")
}

func (ah *apiHandlers) idWriteHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	collection := vars["collection"]

	var doc Document
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&doc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if getId(doc) != id {
		http.Error(w, "id does not match", http.StatusBadRequest)
		return
	}

	err = ah.index.Write(collection, id, doc)
	if err != nil {
		http.Error(w, fmt.Sprintf("write failed:\n%v\n", err), http.StatusInternalServerError)
		return
	}
}

func (ah *apiHandlers) dropHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	collection := vars["collection"]
	ah.index.Drop(collection)
}

func (ah *apiHandlers) searchHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	collection := vars["collection"]

	count := 20
	r.ParseForm()
	max := r.Form["max"]
	if len(max) == 1 {
		i, err := strconv.Atoi(max[0])
		if err == nil {
			count = i
		}
	}
	terms := r.Form["term"]

	stop := make(chan struct{})
	defer close(stop)
	cont, err := ah.index.Search(collection, terms, stop, count)
	if err == ERR_NOT_IMPLEMENTED {
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	}
	if err == ERR_INVALID_QUERY {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	for c := range cont {
		err := enc.Encode(c)
		if err != nil {
			log.Printf("error writing json to response: %v\n", err)
			return
		}
		fmt.Fprint(w, "\n")
	}
}

func (ah *apiHandlers) dumpAll(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	collection := vars["collection"]

	enc := json.NewEncoder(w)
	stop := make(chan struct{})
	defer close(stop)
	all, err := ah.index.All(collection, stop)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for doc := range all {
		enc.Encode(doc)
		fmt.Fprint(w, "\n")
	}
}

func (ah *apiHandlers) countHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	collection := vars["collection"]
	fmt.Fprintf(w, "%d", ah.index.Count(collection))
}