set -e
source calc.sh
[ "$(add 2 3)" = "5" ] || { echo "add(2,3) != 5"; exit 1; }
[ "$(add 10 -4)" = "6" ] || { echo "add(10,-4) != 6"; exit 1; }
echo OK
