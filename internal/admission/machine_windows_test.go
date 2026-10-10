//go:build windows

package admission

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestObserveMachineReadsTheHost protects the Windows reader: a host whose
// memory is readable is observed, and its figure is a positive amount no
// larger than the machine's physical memory. Mutations that fail it: the
// reader left unobserved (every Windows admission then runs against the
// stand-in), or the structure's length left unset or its fields misread,
// which fails the call or reads the wrong figure.
func TestObserveMachineReadsTheHost(t *testing.T) {
	var status memoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	if r, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); r == 0 {
		t.Fatalf("the machine's physical memory cannot be read to check against: %v", err)
	}
	m := ObserveMachine()
	if !m.Observed {
		t.Fatal("Windows available memory is unobserved")
	}
	if m.AvailableBytes <= 0 || uint64(m.AvailableBytes) > status.TotalPhys {
		t.Fatalf("available memory is %d bytes on a machine with %d", m.AvailableBytes, status.TotalPhys)
	}
}

// jobLimitChildEnv marks the re-run of this test binary that reads its own
// available memory from inside a memory-limited job.
const jobLimitChildEnv = "CODECTX_ADMISSION_JOB_LIMIT_CHILD"

// jobMemoryLimit is the job limit the child runs under.
const jobMemoryLimit = 512 << 20

// TestJobMemoryLimitBoundsTheReading proves that a job's memory limit, which
// is how a Windows container's limit is enforced, bounds the figure the
// reader reports to a process inside the job, so a container is never
// admitted past its limit. The parent first requires its own reading to
// exceed the limit, so the test cannot pass because the runner was short of
// memory; it then re-runs this test inside a job limited to jobMemoryLimit,
// and the child must read no more than that. Mutations that fail it: taking
// available physical memory alone, which ignores the job. Should the platform
// not carry the job's limit into the commit figure, this fails, and the
// reader must also take the job's limit less its peak usage.
func TestJobMemoryLimitBoundsTheReading(t *testing.T) {
	if os.Getenv(jobLimitChildEnv) != "" {
		// The child: wait until the parent has placed this process in the
		// job, then report the reading.
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatalf("the parent's go-ahead never came: %v", err)
		}
		m := ObserveMachine()
		fmt.Printf("observed=%t available=%d\n", m.Observed, m.AvailableBytes)
		return
	}

	if m := ObserveMachine(); !m.Observed || m.AvailableBytes <= jobMemoryLimit {
		t.Fatalf("this host reads %+v, which does not exceed the %d-byte job limit, so the limit cannot be told apart from it",
			m, jobMemoryLimit)
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_JOB_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		JobMemoryLimit: jobMemoryLimit,
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatalf("SetInformationJobObject: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^TestJobMemoryLimitBoundsTheReading$", "-test.count=1")
	cmd.Env = append(os.Environ(), jobLimitChildEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("OpenProcess: %v", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, h)
	windows.CloseHandle(h)
	if assignErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("AssignProcessToJobObject: %v", assignErr)
	}
	// Only now, inside the job, does the child take its reading.
	fmt.Fprintln(stdin, "go")
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the child failed: %v\n%s", err, out.String())
	}

	var observed bool
	var available int64
	for line := range strings.Lines(out.String()) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "observed=")
		if !ok {
			continue
		}
		flag, figure, ok := strings.Cut(rest, " available=")
		if !ok {
			continue
		}
		observed = flag == "true"
		if available, err = strconv.ParseInt(figure, 10, 64); err != nil {
			t.Fatalf("the child's reading %q does not parse: %v", line, err)
		}
	}
	if !observed {
		t.Fatalf("inside the job the reading is unobserved:\n%s", out.String())
	}
	if available > jobMemoryLimit {
		t.Fatalf("inside a job limited to %d bytes the child reads %d available", jobMemoryLimit, available)
	}
}
