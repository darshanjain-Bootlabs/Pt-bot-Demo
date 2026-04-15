import http from "k6/http";
import { check, sleep } from "k6";

export const options = {
  vus: 30,
  duration: "15s",
};

export default function () {
  let res = http.get("https://httpbin.org/get?query=k6");

  check(res, {
    "status is 200": (r) => r.status === 200,
  });

  sleep(0.5);
}
