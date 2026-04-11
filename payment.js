import http from "k6/http";
import { check, sleep } from "k6";

export const options = {
  vus: 20,
  duration: "20s",
};

export default function () {
  let payload = JSON.stringify({
    amount: 100,
    currency: "USD",
  });

  let params = {
    headers: {
      "Content-Type": "application/json",
    },
  };

  let res = http.post("https://httpbin.org/post", payload, params);

  check(res, {
    "status is 200": (r) => r.status === 200,
  });

  sleep(1.5);
}
