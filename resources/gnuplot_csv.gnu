#!/usr/bin/env gnuplot -c

# 1. Setup Input (Takes CSV path as an argument, defaults to /tmp/...)
csv_file = ARG1
if (strlen(csv_file) == 0) csv_file = "/tmp/plgm_metrics_export.csv"

print "Loading metrics from: ", csv_file

# 2. Terminal and Styling Setup (Interactive Window)
set terminal qt size 800,600 font "Arial,11"
set datafile separator ","

# 3. Define Official UI Colors (Hex Codes)
set style line 1 lc rgb '#1486ff' pt 7 ps 0.4 lw 2 # Select (Sky)
set style line 2 lc rgb '#30d1b2' pt 7 ps 0.4 lw 2 # Insert (Aqua)
set style line 3 lc rgb '#f0b336' pt 7 ps 0.4 lw 2 # Upsert (Sunrise)
set style line 4 lc rgb '#ff7e1a' pt 7 ps 0.4 lw 2 # Update (Sunset)
set style line 5 lc rgb '#8b5cf6' pt 7 ps 0.4 lw 2 # Delete (Amethyst)
set style line 6 lc rgb '#ec4899' pt 7 ps 0.4 lw 2 # Agg (Pink)
set style line 7 lc rgb '#059669' pt 7 ps 0.4 lw 2 # Trans (Forest)
set style line 8 lc rgb '#a0aec0' lw 2 dt 2        # Iteration Marker (Gray Dashed)
set style line 9 lc rgb '#0e1a53' lw 2             # Total (Night/Dark Blue)

# Grid styling
set grid ytics lc rgb "#e2e8f0" lw 1 lt 0
set grid xtics lc rgb "#e2e8f0" lw 1 lt 0
set key outside right top

# 4. Time Axis Setup
set xdata time
set timefmt "%Y-%m-%dT%H:%M:%S"
set format x "%H:%M:%S"

set xrange [*:*] noextend

# 5. Iteration Marker Function
prev_iter = -1
mark_iter(x) = (x != prev_iter ? (prev_iter = x, 1) : NaN)

# 6. Multiplot Setup (Stacked Graphs)
set multiplot layout 2,1 margins 0.08, 0.85, 0.08, 0.92 spacing 0.1

# ---------------------------------------------------------
# TOP PLOT: Throughput (Ops/sec)
# ---------------------------------------------------------
set title "PLGM Workload Analysis: Throughput" font "Arial:Bold,14" tc rgb "#2d3748"
set ylabel "Operations / Second" tc rgb "#4a5568"
set format y "%g"
set yrange [0:*]

# Setup a secondary Y-axis strictly for drawing the vertical Iteration lines
set y2range [0:1]
unset y2tics

# Reset the iteration tracker right before the plot
prev_iter = -1

# Note: Use 'smooth mcsplines' (Monotonic Cubic Splines) for gentle smoothing without overshooting
# Iteration is now column 19. Total is column 3. Select shifted to 4, etc.
plot csv_file using 1:(mark_iter($19)) every ::1 axes x1y2 with impulses ls 8 title "New Iteration", \
     "" using 1:3 every ::1 smooth mcsplines ls 9 title "Total", \
     "" using 1:4 every ::1 smooth mcsplines ls 1 title "Select", \
     "" using 1:5 every ::1 smooth mcsplines ls 2 title "Insert", \
     "" using 1:6 every ::1 smooth mcsplines ls 3 title "Upsert", \
     "" using 1:7 every ::1 smooth mcsplines ls 4 title "Update", \
     "" using 1:8 every ::1 smooth mcsplines ls 5 title "Delete", \
     "" using 1:9 every ::1 smooth mcsplines ls 6 title "Agg", \
     "" using 1:10 every ::1 smooth mcsplines ls 7 title "Trans"

# ---------------------------------------------------------
# BOTTOM PLOT: P99 Latency by Operation (Log Scale)
# ---------------------------------------------------------
set title "PLGM Workload Analysis: P99 Latency by Operation" font "Arial:Bold,14" tc rgb "#2d3748"
set ylabel "Latency (ms)" tc rgb "#4a5568"
set xlabel "Time (UTC)" tc rgb "#4a5568"

set logscale y 10
set nologscale y2      
set format y "%g" 
set yrange [0.1:*] 
set y2range [0:1]

log_safe(v) = (v > 0.1 ? v : 0.1)

# Reset the iteration tracker again for the second plot
prev_iter = -1

# Iteration is now column 19. Total Latency shifted to 11, Select Latency to 12, etc.
plot csv_file using 1:(mark_iter($19)) every ::1 axes x1y2 with impulses ls 8 title "New Iteration", \
     "" using 1:(log_safe($11)) every ::1 smooth mcsplines ls 9 title "Total P99", \
     "" using 1:(log_safe($12)) every ::1 smooth mcsplines ls 1 title "Select P99", \
     "" using 1:(log_safe($13)) every ::1 smooth mcsplines ls 2 title "Insert P99", \
     "" using 1:(log_safe($14)) every ::1 smooth mcsplines ls 3 title "Upsert P99", \
     "" using 1:(log_safe($15)) every ::1 smooth mcsplines ls 4 title "Update P99", \
     "" using 1:(log_safe($16)) every ::1 smooth mcsplines ls 5 title "Delete P99", \
     "" using 1:(log_safe($17)) every ::1 smooth mcsplines ls 6 title "Agg P99", \
     "" using 1:(log_safe($18)) every ::1 smooth mcsplines ls 7 title "Trans P99"

unset multiplot

print "Interactive graph opened!"

# 7. Pause to keep the interactive window open
pause -1 "Press Enter in your terminal to close the graph..."