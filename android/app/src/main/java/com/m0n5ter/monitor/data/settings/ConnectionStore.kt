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

/** The dashboard password saved for one address, so a saved server reconnects with a tap. */
private fun passwordKey(url: String) = stringPreferencesKey("password:$url")

/**
 * Any one node in the mesh answers for the whole fleet (see README's
 * "Distributed" pitch), so the app only ever needs one active base URL at a
 * time. Several are kept on hand so switching between, say, a home mesh and a
 * work mesh does not mean retyping the address each time.
 *
 * Each address keeps its own password, since different meshes have different
 * ones. They sit in the app's private storage, which is as far as a password
 * that already crosses plain HTTP is worth protecting.
 */
class ConnectionStore(private val context: Context) {

    data class State(val savedUrls: List<String>, val activeUrl: String?, val activePassword: String? = null)

    val state: Flow<State> = context.dataStore.data.map { prefs ->
        val saved = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
        val active = prefs[KEY_ACTIVE_URL]?.takeIf { it.isNotBlank() }
        State(saved, active, active?.let { prefs[passwordKey(it)] })
    }

    /**
     * Makes url the active server. A null password keeps whatever was saved
     * for it - which is what tapping a saved server means - while an empty one
     * clears it.
     */
    suspend fun setActive(url: String, password: String? = null) {
        val normalized = normalize(url)
        context.dataStore.edit { prefs ->
            val existing = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
            val updated = (listOf(normalized) + existing.filterNot { it == normalized })
            prefs[KEY_SAVED_URLS] = updated.joinToString("\n")
            prefs[KEY_ACTIVE_URL] = normalized
            when {
                password == null -> Unit
                password.isEmpty() -> prefs.remove(passwordKey(normalized))
                else -> prefs[passwordKey(normalized)] = password
            }
        }
    }

    suspend fun forget(url: String) {
        context.dataStore.edit { prefs ->
            val existing = prefs[KEY_SAVED_URLS]?.split("\n")?.filter { it.isNotBlank() } ?: emptyList()
            prefs[KEY_SAVED_URLS] = existing.filterNot { it == url }.joinToString("\n")
            prefs.remove(passwordKey(url))
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
