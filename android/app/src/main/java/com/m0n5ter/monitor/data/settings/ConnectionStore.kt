package com.m0n5ter.monitor.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

private val Context.dataStore by preferencesDataStore(name = "connections")

private val KEY_SAVED_URLS = stringPreferencesKey("saved_urls") // "\n"-joined
private val KEY_ACTIVE_URL = stringPreferencesKey("active_url")

/**
 * Any one node in the mesh answers for the whole fleet (see README's
 * "Distributed" pitch), so the app only ever needs one active base URL at a
 * time. Several are kept on hand so switching between, say, a home mesh and a
 * work mesh does not mean retyping the address each time.
 */
class ConnectionStore(private val context: Context) {

    data class State(val savedUrls: List<String>, val activeUrl: String?)

    val state: Flow<State> = context.dataStore.data.map { prefs ->
        val saved = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
        State(saved, prefs[KEY_ACTIVE_URL]?.takeIf { it.isNotBlank() })
    }

    suspend fun setActive(url: String) {
        val normalized = normalize(url)
        context.dataStore.edit { prefs ->
            val existing = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
            val updated = (listOf(normalized) + existing.filterNot { it == normalized })
            prefs[KEY_SAVED_URLS] = updated.joinToString("\n")
            prefs[KEY_ACTIVE_URL] = normalized
        }
    }

    suspend fun forget(url: String) {
        context.dataStore.edit { prefs ->
            val existing = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
            prefs[KEY_SAVED_URLS] = existing.filterNot { it == url }.joinToString("\n")
            if (prefs[KEY_ACTIVE_URL] == url) prefs.remove(KEY_ACTIVE_URL)
        }
    }

    companion object {
        /** Trims whitespace, drops a trailing slash, and defaults to http:// with no scheme given. */
        fun normalize(input: String): String {
            var url = input.trim().trimEnd('/')
            if (!url.startsWith("http://") && !url.startsWith("https://")) {
                url = "http://$url"
            }
            return url
        }
    }
}
