package cwagi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/Xe/ln"
	"github.com/Xe/ln/opname"
	"github.com/Xe/olin/internal/abi/cwa"
	"github.com/Xe/olin/proto/brand"
	"github.com/go-interpreter/wagon/wasm"
	"github.com/golang/protobuf/proto"
	"github.com/kr/pretty"
	"github.com/pborman/uuid"
	"github.com/perlin-network/life/exec"
)

// NewVM creates a new virtual machine with the given WebAssembly binary code and name.
func NewVM(data []byte, argv []string, name, mainFunc string) (*VMServer, error) {
	myID := uuid.New()

	mod, err := wasm.DecodeModule(bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	var b brand.Brand

	for _, cs := range mod.Customs {
		log.Printf("custom section %s", cs.Name)
	}

	if cs := mod.Custom("olin-settings"); cs != nil {
		err = proto.Unmarshal(cs.Data, &b)
		if err != nil {
			return nil, err
		}

		pretty.Println(b)
	}

	if b.Opts == nil {
		b.Opts = &brand.VMOptions{
			EnableJit:    false,
			DefaultPages: 32,
			MaxPages:     48,
			MainFunc:     mainFunc,
		}
	} else {
		log.Printf("loading %s by %s", b.Meta.Name, b.Meta.Author)
	}

	p := cwa.NewProcess(name+"+"+myID, argv, map[string]string{
		"RUN_ID": myID,
	})

	cfg := exec.VMConfig{
		EnableJIT:          b.Opts.EnableJit,
		DefaultMemoryPages: int(b.Opts.DefaultPages),
		MaxMemoryPages:     int(b.Opts.MaxPages),
	}
	vm, err := exec.NewVirtualMachine(data, cfg, p)
	if err != nil {
		return nil, err
	}

	main, ok := vm.GetFunctionExport(b.Opts.MainFunc)
	if !ok {
		return nil, errors.New("cwagi: need main function to be exported")
	}

	return &VMServer{
		VM:       vm,
		P:        p,
		mainFunc: main,
		myID:     myID,
	}, nil
}

type VMServer struct {
	VM       *exec.VirtualMachine
	P        *cwa.Process
	lock     sync.Mutex
	mainFunc int
	myID     string
}

func (v *VMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	stdout := bytes.NewBuffer(nil)
	stdin := r.Body
	defer r.Body.Close()
	ctx := opname.With(r.Context(), "vmServer.ServeHTTP")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runID := uuid.New()

	f := ln.F{
		"main_func":    v.mainFunc,
		"process_name": v.P.Name(),
		"run_id":       runID,
		"method":       r.Method,
		"request_uri":  r.RequestURI,
	}

	v.lock.Lock()
	defer v.lock.Unlock()
	v.P.Stdin = stdin
	v.P.Stdout = stdout
	v.P.Setenv(map[string]string{
		"REQUEST_METHOD": r.Method,
		"REQUEST_URI":    r.RequestURI,
		"QUERY_STRING":   r.URL.Query().Encode(),
		"RUN_ID":         runID,
		"WORKER_ID":      v.myID,
	})

	t0 := time.Now()
	ret, err := v.VM.Run(v.mainFunc)
	if err != nil {
		http.Error(w, "internal server error: VM error, run ID: "+runID, http.StatusInternalServerError)
		go func() {
			time.Sleep(125 * time.Millisecond)
			ln.FatalErr(ctx, err, f)
		}()
		return
	}
	f["exec_dur"] = time.Since(t0)

	if ret != 0 {
		ln.Log(ctx, f, ln.F{
			"return_value": ret,
		})
		http.Error(w, fmt.Sprintf("internal server error: return code %d", ret), http.StatusInternalServerError)
		return
	}

	ctx = opname.With(ctx, "respond")
	resp, err := http.ReadResponse(bufio.NewReader(stdout), r)
	if err != nil {
		ln.Error(ctx, err, f)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// copy headers
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	// copy status code
	w.WriteHeader(resp.StatusCode)
	f["status"] = resp.StatusCode

	// copy body
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		ln.Error(opname.With(ctx, "copy_body"), err, f)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if rand.Int63()%50 == 0 {
		ln.Log(ctx, f, ln.Info("successful invocation"))
	}
}
