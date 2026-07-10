from time import sleep

from walle_bench.utils import timeout

try:
    with timeout(0.5):
        sleep(1)

    raise RuntimeError("Timeout did not occur as expected")
except TimeoutError:
    print("Timeout occurred as expected")

with timeout(1):
    sleep(0.5)

print("No timeout occurred as expected")
