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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.api.models.CreateSandboxRequest
import com.alibaba.opensandbox.sandbox.api.models.LifecycleHook
import com.alibaba.opensandbox.sandbox.api.models.PeriodicLifecycleHook
import com.alibaba.opensandbox.sandbox.api.models.SandboxLifecycle
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class SandboxLifecycleModelsTest {
    @Test
    fun `create request lifecycle round trips through JSON`() {
        val request =
            CreateSandboxRequest(
                lifecycle =
                    SandboxLifecycle(
                        preStart = LifecycleHook(command = listOf("/opt/hooks/restore.sh")),
                        periodic =
                            listOf(
                                PeriodicLifecycleHook(
                                    name = "checkpoint",
                                    schedule = "*/5 * * * *",
                                    command = listOf("/opt/hooks/checkpoint.sh"),
                                ),
                            ),
                    ),
            )

        val decoded = Json.decodeFromString<CreateSandboxRequest>(Json.encodeToString(request))

        assertEquals(request.lifecycle, decoded.lifecycle)
    }
}
