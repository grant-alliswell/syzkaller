// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package csource

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys"
)

type Options struct {
	Threaded bool
	Collide  bool
}

func Write(p *prog.Prog, opts Options) []byte {
	exec := p.SerializeForExec()
	w := new(bytes.Buffer)

	fmt.Fprintf(w, `// autogenerated by syzkaller (http://github.com/google/syzkaller)
#include <unistd.h>
#include <sys/syscall.h>
#include <string.h>
#include <stdint.h>
#include <pthread.h>

`)

	handled := make(map[string]bool)
	for _, c := range p.Calls {
		name := c.Meta.CallName
		nr, ok := prog.NewSyscalls[name]
		if !ok || handled[name] {
			continue
		}
		handled[name] = true
		fmt.Fprintf(w, "#ifndef SYS_%v\n", name)
		fmt.Fprintf(w, "#define SYS_%v %v\n", name, nr)
		fmt.Fprintf(w, "#endif\n")
	}
	fmt.Fprintf(w, "\n")

	calls, nvar := generateCalls(exec)
	fmt.Fprintf(w, "long r[%v];\n\n", nvar)

	if !opts.Threaded && !opts.Collide {
		fmt.Fprintf(w, "int main()\n{\n")
		fmt.Fprintf(w, "\tmemset(r, -1, sizeof(r));\n")
		for _, c := range calls {
			fmt.Fprintf(w, "%s", c)
		}
		fmt.Fprintf(w, "\treturn 0;\n}\n")
	} else {
		fmt.Fprintf(w, "void *thr(void *arg)\n{\n")
		fmt.Fprintf(w, "\tswitch ((long)arg) {\n")
		for i, c := range calls {
			fmt.Fprintf(w, "\tcase %v:\n", i)
			fmt.Fprintf(w, "%s", strings.Replace(c, "\t", "\t\t", -1))
			fmt.Fprintf(w, "\t\tbreak;\n")
		}
		fmt.Fprintf(w, "\t}\n")
		fmt.Fprintf(w, "\treturn 0;\n}\n\n")

		fmt.Fprintf(w, "int main()\n{\n")
		fmt.Fprintf(w, "\tlong i;\n")
		fmt.Fprintf(w, "\tpthread_t th[%v];\n", len(calls))
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "\tmemset(r, -1, sizeof(r));\n")
		fmt.Fprintf(w, "\tfor (i = 0; i < %v; i++) {\n", len(calls))
		fmt.Fprintf(w, "\t\tpthread_create(&th[i], 0, thr, (void*)i);\n")
		fmt.Fprintf(w, "\t\tusleep(10000);\n")
		fmt.Fprintf(w, "\t}\n")
		if opts.Collide {
			fmt.Fprintf(w, "\tfor (i = 0; i < %v; i++) {\n", len(calls))
			fmt.Fprintf(w, "\t\tpthread_create(&th[i], 0, thr, (void*)i);\n")
			fmt.Fprintf(w, "\t\tif (i%%2==0)\n")
			fmt.Fprintf(w, "\t\t\tusleep(10000);\n")
			fmt.Fprintf(w, "\t}\n")
		}
		fmt.Fprintf(w, "\tusleep(100000);\n")
		fmt.Fprintf(w, "\treturn 0;\n}\n")
	}
	return w.Bytes()
}

func generateCalls(exec []byte) ([]string, int) {
	read := func() uintptr {
		if len(exec) < 8 {
			panic("exec program overflow")
		}
		v := *(*uint64)(unsafe.Pointer(&exec[0]))
		exec = exec[8:]
		return uintptr(v)
	}
	resultRef := func() string {
		arg := read()
		res := fmt.Sprintf("r[%v]", arg)
		if opDiv := read(); opDiv != 0 {
			res = fmt.Sprintf("%v/%v", res, opDiv)
		}
		if opAdd := read(); opAdd != 0 {
			res = fmt.Sprintf("%v+%v", res, opAdd)
		}
		return res
	}
	lastCall := 0
	seenCall := false
	var calls []string
	w := new(bytes.Buffer)
	newCall := func() {
		if seenCall {
			seenCall = false
			calls = append(calls, w.String())
			w = new(bytes.Buffer)
		}
	}
	n := 0
loop:
	for ; ; n++ {
		switch instr := read(); instr {
		case prog.ExecInstrEOF:
			break loop
		case prog.ExecInstrCopyin:
			newCall()
			addr := read()
			typ := read()
			size := read()
			switch typ {
			case prog.ExecArgConst:
				arg := read()
				fmt.Fprintf(w, "\t*(uint%v_t*)0x%x = (uint%v_t)0x%x;\n", size*8, addr, size*8, arg)
			case prog.ExecArgResult:
				fmt.Fprintf(w, "\t*(uint%v_t*)0x%x = %v;\n", size*8, addr, resultRef())
			case prog.ExecArgData:
				data := exec[:size]
				exec = exec[(size+7)/8*8:]
				var esc []byte
				for _, v := range data {
					hex := func(v byte) byte {
						if v < 10 {
							return '0' + v
						}
						return 'a' + v - 10
					}
					esc = append(esc, '\\', 'x', hex(v>>4), hex(v<<4>>4))
				}
				fmt.Fprintf(w, "\tmemcpy((void*)0x%x, \"%s\", %v);\n", addr, esc, size)
			default:
				panic("bad argument type")
			}
		case prog.ExecInstrCopyout:
			addr := read()
			size := read()
			fmt.Fprintf(w, "\tif (r[%v] != -1)\n", lastCall)
			fmt.Fprintf(w, "\t\tr[%v] = *(uint%v_t*)0x%x;\n", n, size*8, addr)
		case prog.ExecInstrSetPad:
			newCall()
			read() // addr
			read() // size
		case prog.ExecInstrCheckPad:
			read() // addr
			read() // size
		default:
			// Normal syscall.
			newCall()
			meta := sys.Calls[instr]
			fmt.Fprintf(w, "\tr[%v] = syscall(SYS_%v", n, meta.CallName)
			nargs := read()
			for i := uintptr(0); i < nargs; i++ {
				typ := read()
				size := read()
				_ = size
				switch typ {
				case prog.ExecArgConst:
					fmt.Fprintf(w, ", 0x%xul", read())
				case prog.ExecArgResult:
					fmt.Fprintf(w, ", %v", resultRef())
				default:
					panic("unknown arg type")
				}
			}
			for i := nargs; i < 6; i++ {
				fmt.Fprintf(w, ", 0")
			}
			fmt.Fprintf(w, ");\n")
			lastCall = n
			seenCall = true
		}
	}
	newCall()
	return calls, n
}

// WriteTempFile writes data to a temp file and returns its name.
func WriteTempFile(data []byte) (string, error) {
	f, err := ioutil.TempFile("", "syz-prog")
	if err != nil {
		return "", fmt.Errorf("failed to create a temp file: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("failed to write temp file: %v", err)
	}
	f.Close()
	return f.Name(), nil
}

// Build builds a C/C++ program from source file src
// and returns name of the resulting binary.
func Build(src string) (string, error) {
	bin, err := ioutil.TempFile("", "syzkaller")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	bin.Close()
	out, err := exec.Command("gcc", "-x", "c++", "-std=gnu++11", src, "-o", bin.Name(), "-lpthread", "-static", "-O1", "-g").CombinedOutput()
	if err != nil {
		os.Remove(bin.Name())
		data, _ := ioutil.ReadFile(src)
		return "", fmt.Errorf("failed to build program::\n%s\n%s", data, out)
	}
	return bin.Name(), nil
}