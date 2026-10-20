#!/bin/sh
awk -F, 'NR > 1 { n++ } END { print n " rows" }' "$1"
