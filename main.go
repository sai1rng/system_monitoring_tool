package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

// processInfo holds the name, start time, and parent PID for a tracked process.
type processInfo struct {
	Name      string
	StartTime time.Time
	PPID      int32
}

// dataRecord holds all the information we want to save for an event.
type dataRecord struct {
	Timestamp             string
	PID                   int32
	PPID                  int32
	ThreadID              int32 // The ID of the specific thread
	ProcessName           string
	ProcessStartTime      string // The time the process was created
	ProcessPlatformStatus string // Status from gopsutil (e.g., "running", "sleeping")
	NumThreads            int32
	Status                string    // "Started", "Exited", "Active", "Heartbeat"
	Duration              string    // Total runtime, only for "Exited" status
	CPUPercent            float64   // Process-level CPU usage
	MemoryPercent         float32   // Process-level Memory usage
	ThreadUserTime        float64   // Cumulative user time for the thread
	ThreadSystemTime      float64   // Cumulative system time for the thread
	OverallSystemCPU      float64   // Overall system CPU usage
	PerCoreSystemCPU      []float64 // Per-core system CPU usage
	TotalSystemMemUsedGB  float64
}

const (
	timestampFormat = "2006-01-02T15:04:05.000Z07:00"
)

var (
	// activeProcesses tracks all running processes that we've seen, mapping PID to its info.
	activeProcesses = make(map[int32]processInfo)
	// csvFilename is set at runtime to be unique for each execution.
	csvFilename string
	// logInterval is how often to collect data, set via command-line flag.
	logInterval time.Duration
)

func main() {
	verbose := flag.Bool("verbose", false, "Enable verbose logging to standard error.")
	flag.DurationVar(&logInterval, "interval", 1*time.Millisecond, "How often to collect data (e.g., '500ms', '1s')")
	flag.Parse()

	if !*verbose {
		log.SetOutput(io.Discard)
	}

	// Set a unique filename at the start of the program.
	csvFilename = fmt.Sprintf("system_log_%s.csv", time.Now().Format("20060102_150405"))

	log.Println("Starting system monitor...")
	log.Printf("Data will be collected every %v and saved to %s", logInterval, csvFilename)
	// Always show essential startup information using fmt.
	fmt.Println("Starting system monitor...")
	fmt.Printf("Data will be collected every %v and saved to %s\n", logInterval, csvFilename)

	// This log message will only appear in verbose mode.
	log.Printf("Detected %d CPU cores.", runtime.NumCPU())

	initializeCSV()
	populateInitialProcesses()

	ticker := time.NewTicker(logInterval)
	defer ticker.Stop()

	// Run once immediately, then wait for the ticker.
	collectAndLogData()

	for {
		select {
		case <-ticker.C:
			collectAndLogData()
		}
	}
}

// initializeCSV creates the CSV file and writes the dynamic header row.
func initializeCSV() {
	// Since the filename is unique per run, we always create a new file.
	// No need to check if it exists.
	log.Printf("Creating new CSV file: %s", csvFilename)
	file, err := os.Create(csvFilename)
	if err != nil {
		log.Fatalf("Failed to create CSV file: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Build Dynamic Headers
	headers := []string{
		"Timestamp",
		"PID",
		"ParentPID",
		"ThreadID",
		"ProcessName",
		"ProcessStartTime",
		"ProcessPlatformStatus",
		"NumThreads",
		"Status",
		"Duration",
		"ProcessCPUPercent",
		"ProcessMemoryPercent",
		"ThreadUserTime",
		"ThreadSystemTime",
		"OverallSystemCPUUsagePercent",
	}

	numCPU := runtime.NumCPU()
	for i := 0; i < numCPU; i++ {
		headers = append(headers, fmt.Sprintf("Core%d_Percent", i))
	}
	headers = append(headers, "TotalSystemMemoryUsedGB")

	if err := writer.Write(headers); err != nil {
		log.Fatalf("Failed to write headers to CSV: %v", err)
	}
}

// populateInitialProcesses gets the initial list of running processes.
func populateInitialProcesses() {
	processes, err := process.Processes()
	if err != nil {
		log.Printf("Warning: Could not get initial process list: %v", err)
		return
	}
	for _, p := range processes {
		name, err := p.Exe()
		if err != nil {
			name = "N/A"
		}

		ppid, err := p.Ppid()
		if err != nil {
			ppid = 0
		}

		createTimeMs, err := p.CreateTime()
		var startTime time.Time
		if err != nil {
			startTime = time.Now() // Fallback if create time is unavailable
		} else {
			startTime = time.Unix(0, createTimeMs*int64(time.Millisecond))
		}

		activeProcesses[p.Pid] = processInfo{
			Name:      name,
			StartTime: startTime,
			PPID:      ppid,
		}
	}
	log.Printf("Initial scan complete. Tracking %d running processes.", len(activeProcesses))
}

// collectAndLogData gathers stats, detects process changes, and logs data for all active processes.
func collectAndLogData() {
	log.Println("Collecting data...")
	var records []dataRecord
	currentTime := time.Now()

	// --- 1. Get Total System Stats ---
	overallPercentages, _ := cpu.Percent(time.Millisecond, false)
	perCorePercentages, _ := cpu.Percent(time.Millisecond, true)
	vm, _ := mem.VirtualMemory()
	overallCPUUsage := overallPercentages[0]
	totalMemUsedGB := float64(vm.Used) / (1024 * 1024 * 1024)

	// --- 2. Check for Process Changes and Log Data ---
	allProcs, err := process.Processes()
	if err != nil {
		log.Printf("Error getting process list: %v", err)
		return
	}

	currentPIDs := make(map[int32]*process.Process)
	for _, p := range allProcs {
		currentPIDs[p.Pid] = p
	}

	// Check for EXITED processes
	for pid, info := range activeProcesses {
		if _, exists := currentPIDs[pid]; !exists {
			duration := time.Since(info.StartTime).Round(time.Millisecond).String()
			records = append(records, dataRecord{
				Timestamp:        currentTime.Format(timestampFormat),
				PID:              pid,
				PPID:             info.PPID,
				ProcessName:      info.Name,
				ProcessStartTime: info.StartTime.Format(timestampFormat),
				Status:           "Exited",
				Duration:         duration,
			})
			delete(activeProcesses, pid) // Remove from our tracking map
		}
	}

	// Log ACTIVE processes and check for NEW processes
	for pid, p := range currentPIDs {
		cpuP, _ := p.CPUPercent()
		memP, _ := p.MemoryPercent()
		ppid, _ := p.Ppid()
		numThreads, _ := p.NumThreads()
		processStatuses, err := p.Status()
		var platformStatus string
		if err != nil || len(processStatuses) == 0 {
			platformStatus = "N/A"
		} else {
			platformStatus = processStatuses[0]
		}

		threads, err := p.Threads()
		if err != nil {
			log.Printf("Warning: Could not get threads for PID %d: %v", pid, err)
		}

		if info, exists := activeProcesses[pid]; exists {
			// It's an ACTIVE, already tracked process. Log its current state.
			if len(threads) > 0 {
				for tid, threadStat := range threads {
					records = append(records, dataRecord{
						Timestamp:             currentTime.Format(timestampFormat),
						PID:                   pid,
						PPID:                  ppid,
						ThreadID:              tid,
						ProcessName:           info.Name, // Use the stored name for consistency
						ProcessStartTime:      info.StartTime.Format(timestampFormat),
						ProcessPlatformStatus: platformStatus,
						NumThreads:            numThreads,
						Status:                "Active",
						CPUPercent:            cpuP,
						MemoryPercent:         memP,
						ThreadUserTime:        threadStat.User,
						ThreadSystemTime:      threadStat.System,
						OverallSystemCPU:      overallCPUUsage,
						PerCoreSystemCPU:      perCorePercentages,
						TotalSystemMemUsedGB:  totalMemUsedGB,
					})
				}
			} else {
				// If no threads are found (or on error), log a single process-level record.
				records = append(records, dataRecord{
					Timestamp:             currentTime.Format(timestampFormat),
					PID:                   pid,
					PPID:                  ppid,
					ProcessName:           info.Name,
					ProcessStartTime:      info.StartTime.Format(timestampFormat),
					ProcessPlatformStatus: platformStatus,
					NumThreads:            numThreads,
					Status:                "Active",
					CPUPercent:            cpuP,
					MemoryPercent:         memP,
					OverallSystemCPU:      overallCPUUsage,
					PerCoreSystemCPU:      perCorePercentages,
					TotalSystemMemUsedGB:  totalMemUsedGB,
				})
			}
		} else {
			// It's a NEW process. Log it as "Started".
			name, _ := p.Exe()
			createTimeMs, err := p.CreateTime()
			var startTime time.Time
			if err != nil {
				startTime = currentTime // Fallback to current time
			} else {
				startTime = time.Unix(0, createTimeMs*int64(time.Millisecond))
			}

			if len(threads) > 0 {
				for tid, threadStat := range threads {
					records = append(records, dataRecord{
						Timestamp:             currentTime.Format(timestampFormat),
						PID:                   pid,
						PPID:                  ppid,
						ThreadID:              tid,
						ProcessName:           name,
						ProcessStartTime:      startTime.Format(timestampFormat),
						ProcessPlatformStatus: platformStatus,
						NumThreads:            numThreads,
						Status:                "Started",
						CPUPercent:            cpuP,
						MemoryPercent:         memP,
						ThreadUserTime:        threadStat.User,
						ThreadSystemTime:      threadStat.System,
						OverallSystemCPU:      overallCPUUsage,
						PerCoreSystemCPU:      perCorePercentages,
						TotalSystemMemUsedGB:  totalMemUsedGB,
					})
				}
			} else {
				// If no threads are found (or on error), log a single process-level record.
				records = append(records, dataRecord{
					Timestamp:             currentTime.Format(timestampFormat),
					PID:                   pid,
					PPID:                  ppid,
					ProcessName:           name,
					ProcessStartTime:      startTime.Format(timestampFormat),
					ProcessPlatformStatus: platformStatus,
					NumThreads:            numThreads,
					Status:                "Started",
					CPUPercent:            cpuP,
					MemoryPercent:         memP,
					OverallSystemCPU:      overallCPUUsage,
					PerCoreSystemCPU:      perCorePercentages,
					TotalSystemMemUsedGB:  totalMemUsedGB,
				})
			}
			// Add to our tracking map
			activeProcesses[pid] = processInfo{Name: name, StartTime: startTime, PPID: ppid}
		}
	}

	// --- 3. Write to CSV ---
	if len(records) > 0 {
		if err := appendToCSV(records); err != nil {
			log.Printf("Error writing to CSV: %v", err)
		}
		log.Printf("Successfully logged %d records (active, started, exited).", len(records))
	} else {
		log.Println("No running processes found. Logging heartbeat.")
		// Log a "heartbeat" if there are no processes to log at all
		heartbeatRecord := []dataRecord{{
			Timestamp:            currentTime.Format(timestampFormat),
			ProcessName:          "SYSTEM_HEARTBEAT",
			Status:               "Heartbeat",
			OverallSystemCPU:     overallCPUUsage,
			PerCoreSystemCPU:     perCorePercentages,
			TotalSystemMemUsedGB: totalMemUsedGB,
		}}
		if err := appendToCSV(heartbeatRecord); err != nil {
			log.Printf("Error writing heartbeat to CSV: %v", err)
		}
	}
}

// appendToCSV appends records to the CSV file.
func appendToCSV(data []dataRecord) error {
	file, err := os.OpenFile(csvFilename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open CSV file: %w", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	for _, record := range data {
		// Start with the base columns
		row := []string{
			record.Timestamp,
			strconv.Itoa(int(record.PID)),
			strconv.Itoa(int(record.PPID)),
			strconv.Itoa(int(record.ThreadID)),
			record.ProcessName,
			record.ProcessStartTime,
			record.ProcessPlatformStatus,
			strconv.Itoa(int(record.NumThreads)),
			record.Status,
			record.Duration,
			strconv.FormatFloat(record.CPUPercent, 'f', 2, 64),
			strconv.FormatFloat(float64(record.MemoryPercent), 'f', 2, 32),
			strconv.FormatFloat(record.ThreadUserTime, 'f', 2, 64),
			strconv.FormatFloat(record.ThreadSystemTime, 'f', 2, 64),
			strconv.FormatFloat(record.OverallSystemCPU, 'f', 2, 64),
		}

		// Append the per-core CPU data
		numCores := runtime.NumCPU()
		if len(record.PerCoreSystemCPU) > 0 {
			for _, corePercent := range record.PerCoreSystemCPU {
				row = append(row, strconv.FormatFloat(corePercent, 'f', 2, 64))
			}
		} else {
			// Add empty placeholders if there's no per-core data (e.g., for "Exited" events)
			for i := 0; i < numCores; i++ {
				row = append(row, "")
			}
		}

		// Append the final memory column
		row = append(row, strconv.FormatFloat(record.TotalSystemMemUsedGB, 'f', 2, 64))

		if err := writer.Write(row); err != nil {
			log.Printf("Failed to write record %+v to CSV: %v", record, err)
		}
	}
	return writer.Error()
}
