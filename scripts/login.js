import http from "k6/http";
import { check, sleep } from "k6";

export const options = {
  vus: 10,
  duration: "10s",
};

export default function () {
  let res = http.get("https://httpbin.org/get");

  check(res, {
    "status is 200": (r) => r.status === 200,
  });

  sleep(1);
}
