/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
 * opensandbox-session-gate is the last fail-closed barrier before a session
 * workload executes. bubblewrap's --block-fd treats EOF as success, so it
 * cannot be the authorization boundary by itself.
 *
 * The helper announces that it is blocked over an inherited SOCK_SEQPACKET
 * socket. execd authenticates the sender with SCM_CREDENTIALS, validates its
 * host PID and network namespace, and then sends the exact READY frame.
 * EOF, a short/long frame, an invalid descriptor, or any socket error exits
 * without ever executing the workload.
 */

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <signal.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#define GATE_FAILURE 125
#define EXEC_FAILURE 126

static const char waiting_frame[] = "OPENSANDBOX_SESSION_WAITING_V1";
static const char ready_frame[] = "OPENSANDBOX_SESSION_READY_V1";

static void fail_closed(int control_fd)
{
    if (control_fd >= 0)
        (void)close(control_fd);
    _exit(GATE_FAILURE);
}

int main(int argc, char **argv)
{
    char *end = NULL;
    long parsed_fd;
    int control_fd;
    int socket_type = 0;
    socklen_t socket_type_len = sizeof(socket_type);
    char incoming[sizeof(ready_frame)];
    ssize_t received;
    ssize_t sent;

    if (argc < 4 || strcmp(argv[2], "--") != 0)
        fail_closed(-1);

    errno = 0;
    parsed_fd = strtol(argv[1], &end, 10);
    if (errno != 0 || end == argv[1] || *end != '\0' ||
        parsed_fd < 3 || parsed_fd > INT_MAX)
        fail_closed(-1);
    control_fd = (int)parsed_fd;

    if (fcntl(control_fd, F_GETFD) < 0)
        fail_closed(control_fd);
    if (getsockopt(control_fd, SOL_SOCKET, SO_TYPE,
                   &socket_type, &socket_type_len) < 0 ||
        socket_type != SOCK_SEQPACKET)
        fail_closed(control_fd);

    (void)signal(SIGPIPE, SIG_IGN);
    sent = send(control_fd, waiting_frame, sizeof(waiting_frame) - 1,
                MSG_NOSIGNAL);
    if (sent != (ssize_t)(sizeof(waiting_frame) - 1))
        fail_closed(control_fd);

    /*
     * The buffer has room for one byte beyond the expected payload. This
     * makes overlong seqpacket messages observably different from READY.
     */
    received = recv(control_fd, incoming, sizeof(incoming), 0);
    if (received != (ssize_t)(sizeof(ready_frame) - 1) ||
        memcmp(incoming, ready_frame, sizeof(ready_frame) - 1) != 0)
        fail_closed(control_fd);

    if (close(control_fd) != 0)
        _exit(GATE_FAILURE);

    execvp(argv[3], &argv[3]);
    _exit(EXEC_FAILURE);
}
