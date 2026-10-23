#define dlopen fixture_dlopen
#define dlsym fixture_dlsym
#define dlclose fixture_dlclose
#include "../station_native.c"
#include <assert.h>
#include <stdlib.h>
#include <string.h>

enum {
  CONF_READ = 1,
  RATE = 2,
  CREATE = 4,
  DESTROY = 8,
  CONF_FREE = 16,
  SUBMIT = 32,
  EVENT = 64
};
static int conf, detector, failure, failures;
static int freed_errors, freed_confs, destroyed, closed_libraries;

static void maybe_fail(int operation, void **err) {
  assert(*err == NULL);
  if (failures & operation)
    *err = &failure;
}

static const char *fixture_forbidden_error_message(const void *err) {
  assert(err == &failure);
  abort();
  return "synthetic-private-token-and-transcript";
}

static void fixture_error_free(void **err) {
  assert(*err == &failure);
  *err = NULL;
  freed_errors++;
}

static void *fixture_conf_read(const char *model, void **err) {
  assert(strcmp(model, "fixture") == 0 && *err == NULL);
  maybe_fail(CONF_READ, err);
  return &conf;
}

static uint32_t fixture_sample_rate(const void *model, void **err) {
  assert(model == &conf && *err == NULL);
  maybe_fail(RATE, err);
  return 16000;
}

static void *fixture_create(const void *model, uint32_t rate, uint32_t option,
                            void **err) {
  assert(model == &conf && rate == 16000 && option == 0);
  assert(err != NULL && *err == NULL);
  maybe_fail(CREATE, err);
  return &detector;
}

static void fixture_destroy(void *value, void **err) {
  assert(value == &detector);
  destroyed++;
  maybe_fail(DESTROY, err);
}

static void fixture_conf_free(void *model, void **err) {
  assert(model == &conf && *err == NULL);
  freed_confs++;
  maybe_fail(CONF_FREE, err);
}

static void fixture_submit(void *value, const void *pcm, size_t bytes,
                           void **err) {
  assert(value == &detector && pcm && bytes == 640);
  maybe_fail(SUBMIT, err);
}

static int32_t fixture_next_event(void *value, void **err) {
  assert(value == &detector);
  maybe_fail(EVENT, err);
  return 0;
}

static void fixture_unused(void) { abort(); }

void *fixture_dlopen(const char *path, int flags) {
  (void)path;
  (void)flags;
  return &conf;
}

int fixture_dlclose(void *library) {
  assert(library == &conf);
  closed_libraries++;
  return 0;
}

void *fixture_dlsym(void *library, const char *symbol) {
  assert(library == &conf);
  if (strcmp(symbol, "spotter_error_get_message") == 0)
    return (void *)fixture_forbidden_error_message;
  if (strcmp(symbol, "spotter_error_free") == 0)
    return (void *)fixture_error_free;
  if (strcmp(symbol, "spotter_conf_read") == 0)
    return (void *)fixture_conf_read;
  if (strcmp(symbol, "spotter_conf_native_sample_rate") == 0)
    return (void *)fixture_sample_rate;
  if (strcmp(symbol, "spotter_create") == 0)
    return (void *)fixture_create;
  if (strcmp(symbol, "spotter_conf_free") == 0)
    return (void *)fixture_conf_free;
  if (strcmp(symbol, "spotter_submit_raw_data") == 0)
    return (void *)fixture_submit;
  if (strcmp(symbol, "spotter_get_next_event") == 0)
    return (void *)fixture_next_event;
  if (strcmp(symbol, "spotter_destroy") == 0)
    return (void *)fixture_destroy;
  return (void *)fixture_unused;
}

int main(void) {
  const int constructors[] = {CONF_READ, RATE, CREATE};
  const char *primary[] = {"configuration read failed", "sample rate failed",
                           "create failed"};
  for (size_t i = 0; i < 3; i++) {
    for (int cleanup = 0; cleanup < 4; cleanup++) {
      failures = constructors[i] | ((cleanup & 1) ? DESTROY : 0) |
                 ((cleanup & 2) ? CONF_FREE : 0);
      freed_errors = freed_confs = destroyed = closed_libraries = 0;
      char error[512] = {0};
      assert(station_spotter_open("fixture", error, sizeof(error)) == NULL);
      assert(strstr(error, primary[i]) == error + strlen("native spotter "));
      assert(!strstr(error, "private"));
      int has_detector = constructors[i] == CREATE;
      assert(destroyed == has_detector && freed_confs == 1 &&
             closed_libraries == 3);
      assert(freed_errors == 1 + ((has_detector && (cleanup & 1)) ? 1 : 0) +
                                 ((cleanup & 2) ? 1 : 0));
      assert((strstr(error, "destroy failed") != NULL) ==
             !!(has_detector && (cleanup & 1)));
      assert((strstr(error, "configuration cleanup failed") != NULL) ==
             !!(cleanup & 2));
    }
  }
  for (int cleanup = 0; cleanup < 4; cleanup++) {
    failures = ((cleanup & 1) ? DESTROY : 0) | ((cleanup & 2) ? CONF_FREE : 0);
    freed_errors = freed_confs = destroyed = closed_libraries = 0;
    char error[512] = {0};
    station_spotter *s = station_spotter_open("fixture", error, sizeof(error));
    assert(s && !error[0]);
    assert(station_spotter_close(s, error, sizeof(error)) ==
           (cleanup ? -1 : 0));
    assert(destroyed == 1 && freed_confs == 1 && closed_libraries == 3);
    assert(freed_errors == ((cleanup & 1) ? 1 : 0) + ((cleanup & 2) ? 1 : 0));
    assert((strstr(error, "destroy failed") != NULL) == !!(cleanup & 1));
    assert((strstr(error, "configuration cleanup failed") != NULL) ==
           !!(cleanup & 2));
  }
  const int feed_failures[] = {SUBMIT, EVENT};
  const char *feed_errors[] = {"native spotter submit failed",
                               "native spotter event read failed"};
  for (size_t i = 0; i < 2; i++) {
    failures = 0;
    freed_errors = 0;
    char error[512] = {0};
    int16_t pcm[320] = {0};
    station_spotter *s = station_spotter_open("fixture", error, sizeof(error));
    assert(s && !error[0]);
    failures = feed_failures[i];
    assert(station_spotter_feed(s, pcm, sizeof(pcm), error, sizeof(error)) ==
           -1);
    assert(strcmp(error, feed_errors[i]) == 0 && freed_errors == 1);
    assert(station_spotter_close(s, error, sizeof(error)) == 0);
  }
  puts("Native errors sanitized; primary and cleanup failures retained; all "
       "cleanup attempted.");
  return 0;
}
