# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Diagnostic mitmproxy addon for the SSE truncation repro.

Logs the responseheaders/response/error hooks of each flow so the truncation
root cause (an HTTP/1 protocol error instead of a clean response end) is
visible in the mitmdump log:

    PROBE error hook fired: HTTP/1 protocol error: peer closed connection
    without sending complete message body (incomplete chunked read)

This is the symptom of an upstream that closes its connection with unread
request data (kernel sends TCP RST, which flushes the receiver's kernel
receive buffer and loses the tail of the response) — see the repro README.

Load with: mitmdump ... -s probe.py
"""

from mitmproxy import ctx, http


def responseheaders(flow: http.HTTPFlow) -> None:
    if flow.response is not None:
        ctx.log.info(f"PROBE responseheaders stream={flow.response.stream}")


def response(flow: http.HTTPFlow) -> None:
    ctx.log.info("PROBE response hook fired")


def error(flow: http.HTTPFlow) -> None:
    ctx.log.info(f"PROBE error hook fired: {flow.error}")
