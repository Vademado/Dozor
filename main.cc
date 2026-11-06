// HelloDetector: YOLOv8 RKNN, камера, светодиод USER и кадр с рамками.
// Основа: Daniilslep/LuckFox_YOLO, cpp/HelloDetector/src/main.cc
// https://github.com/Daniilslep/LuckFox_YOLO/blob/main/cpp/HelloDetector/src/main.cc
// Исходный пример основан на ret7020/Yolov8CustomNPU.
//
// Изменения: удалён OLED; добавлен фильтр COCO person (0) и car (2).
// Модель и model/labels.txt остаются на 80 классов, OBJ_CLASS_NUM = 80.
// Трекинг, таймеры, анализ поведения и уведомления здесь не реализованы.

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <chrono>
#include "yolov8.h"
#include <opencv2/core/core.hpp>
#include <opencv2/highgui/highgui.hpp>
#include <opencv2/imgproc/imgproc.hpp>

#define MODEL_INPUT_SIZE 640     // Должен совпадать с размером входа RKNN.
#define CAMERA_DEV 11            // Номер камеры из исходного примера.
#define CONF_THRESHOLD 0.50f     // Минимальная уверенность детекции.
#define NMS_THRESHOLD 0.30f      // Порог удаления дублирующих рамок.

// Сохранено значение из исходника. Для своей ревизии Pico Ultra
// проверьте GPIO и активный уровень USER LED по схеме платы.
#define LED_GPIO 34

/* ---------------------------- Светодиод USER ---------------------------- */

static void led_init()
{
    FILE *f = fopen("/sys/class/gpio/export", "w");
    if (f) { fprintf(f, "%d", LED_GPIO); fclose(f); }
    char path[64];
    snprintf(path, sizeof(path), "/sys/class/gpio/gpio%d/direction", LED_GPIO);
    f = fopen(path, "w");
    if (f) { fprintf(f, "out"); fclose(f); }
}

static void led_set(int on)
{
    char path[64];
    snprintf(path, sizeof(path), "/sys/class/gpio/gpio%d/value", LED_GPIO);
    FILE *f = fopen(path, "w");
    if (!f) return;
    fprintf(f, "%d", on ? 1 : 0);
    fclose(f);
}

/* ------------------------- Фильтрация детекций -------------------------- */

static void keep_people_and_cars(object_detect_result_list *od)
{
    // Это номера КЛАССОВ COCO, а не индивидуальные track ID.
    // 0 = person (человек), 2 = car (легковой автомобиль).
    // Сеть вычисляет все 80 классов, но приложение использует только эти два.
    int kept = 0;
    for (int i = 0; i < od->count; ++i)
    {
        const int class_id = od->results[i].cls_id;
        if (class_id == 0 || class_id == 2)
        {
            // Переносим всю запись: рамку, уверенность и исходный class ID.
            // kept <= i, поэтому ещё не прочитанные записи не затираются.
            od->results[kept] = od->results[i];
            ++kept;
        }
    }

    // Отрисовка, лог, счётчик и светодиод используют уже этот список.
    // Если людей и автомобилей нет, count будет равен нулю.
    od->count = kept;
}

/* ------------------------------ Детекция ------------------------------- */

static void draw_and_print(cv::Mat &frame, object_detect_result_list &od_results)
{
    for (int i = 0; i < od_results.count; i++)
    {
        object_detect_result *det_result = &(od_results.results[i]);
        int x1 = det_result->box.left;
        int y1 = det_result->box.top;
        int x2 = det_result->box.right;
        int y2 = det_result->box.bottom;
        const char *name = coco_cls_to_name(det_result->cls_id);
        printf("  cls=%d p=%.2f box=%d,%d,%d,%d %s\n",
               det_result->cls_id, det_result->prop, x1, y1, x2, y2, name);
        cv::rectangle(frame, cv::Rect(x1, y1, x2 - x1, y2 - y1), cv::Scalar(0, 255, 0), 2);
        char label[64];
        snprintf(label, sizeof(label), "%s %.2f", name, det_result->prop);
        int ty = y1 > 18 ? y1 - 6 : y1 + 16;
        cv::putText(frame, label, cv::Point(x1, ty),
                    cv::FONT_HERSHEY_SIMPLEX, 0.55, cv::Scalar(0, 255, 0), 2);
    }
}

static int run_on_frame(rknn_app_context_t *ctx, cv::Mat &bgr_frame, cv::Mat &input640,
                        object_detect_result_list *od_out)
{
    // Подготовка кадра и запуск NPU сохранены из исходного примера.
    // input640 использует память входного тензора модели.
    cv::Mat rgb;
    cv::cvtColor(bgr_frame, rgb, cv::COLOR_BGR2RGB);
    cv::resize(rgb, input640, cv::Size(MODEL_INPUT_SIZE, MODEL_INPUT_SIZE), 0, 0, cv::INTER_LINEAR);
    rknn_run(ctx->rknn_ctx, nullptr);
    post_process(ctx, ctx->output_mems, CONF_THRESHOLD, NMS_THRESHOLD, od_out);

    // Фильтруем ДО отрисовки и возврата количества объектов.
    keep_people_and_cars(od_out);
    draw_and_print(bgr_frame, *od_out);
    return od_out->count;
}

int main(int argc, char **argv)
{
    // Построчный вывод позволяет сразу видеть детекции через лог/PC-монитор.
    setvbuf(stdout, nullptr, _IOLBF, 0);

    if (argc != 2 && argc != 3)
    {
        printf("%s <model_path> [image.jpg]\n", argv[0]);
        return -1;
    }

    const char *model_path = argv[1];
    const char *image_path = (argc == 3) ? argv[2] : nullptr;

    int ret;
    rknn_app_context_t rknn_app_ctx;
    memset(&rknn_app_ctx, 0, sizeof(rknn_app_context_t));

    led_init();
    led_set(0);

    cv::VideoCapture cap;
    cv::Mat camFrame;

    if (!image_path)
    {
        printf("opening camera %d...\n", CAMERA_DEV);
        if (!cap.open(CAMERA_DEV))
        {
            printf("camera open failed - проверьте номер камеры и не занята ли она rkipc\n");
            return -1;
        }
        printf("camera ok\n");
        cap >> camFrame;
        if (camFrame.empty())
        {
            printf("empty first frame\n");
            return -1;
        }
        printf("first frame %dx%d\n", camFrame.cols, camFrame.rows);
    }

    printf("init_post_process...\n");
    init_post_process();

    printf("init_yolov8_model %s...\n", model_path);
    ret = init_yolov8_model(model_path, &rknn_app_ctx);
    if (ret != 0)
    {
        printf("init_yolov8_model fail! ret=%d model_path=%s\n", ret, model_path);
        return -1;
    }
    printf("model ok\n");

    cv::Mat bgr640(MODEL_INPUT_SIZE, MODEL_INPUT_SIZE, CV_8UC3, rknn_app_ctx.input_mems[0]->virt_addr);
    object_detect_result_list od_results;

    // Сохранён режим проверки на одном изображении вместо камеры.
    if (image_path)
    {
        camFrame = cv::imread(image_path, 1);
        if (camFrame.empty())
        {
            printf("imread failed: %s\n", image_path);
            return -1;
        }
        printf("image %s %dx%d\n", image_path, camFrame.cols, camFrame.rows);
        run_on_frame(&rknn_app_ctx, camFrame, bgr640, &od_results);
        led_set(od_results.count > 0);
        cv::imwrite("out.jpg", camFrame);
        printf("wrote out.jpg\n");
        return 0;
    }

    while (1)
    {
        std::chrono::steady_clock::time_point begin = std::chrono::steady_clock::now();
        cap >> camFrame;
        if (camFrame.empty())
        {
            printf("empty frame\n");
            continue;
        }
        std::chrono::steady_clock::time_point end = std::chrono::steady_clock::now();
        double cam_lat = std::chrono::duration<double>(end - begin).count();

        begin = std::chrono::steady_clock::now();
        int ndet = run_on_frame(&rknn_app_ctx, camFrame, bgr640, &od_results);
        end = std::chrono::steady_clock::now();
        double infer_lat = std::chrono::duration<double>(end - begin).count();

        // Формат вывода сохранён для совместимости с PC-монитором автора.
        // fps здесь, как в исходнике, не учитывает захват и сохранение кадра.
        printf("cam=%.3f infer=%.3f fps=%.1f dets=%d\n",
               cam_lat, infer_lat, (infer_lat > 0 ? 1.0 / infer_lat : 0.0), ndet);

        // Светодиод реагирует только на оставленные фильтром person/car.
        led_set(ndet > 0);

        // Сохраняем просмотр через PC-монитор. Это обновление JPEG,
        // а не запись видео. Пути и период обновления оставлены как у автора.
        static int frame_i = 0;
        if ((frame_i++ % 3) == 0)
            cv::imwrite("/tmp/out.jpg", camFrame);
        if ((frame_i % 15) == 1)
            cv::imwrite("out.jpg", camFrame);
    }

    return 0;
}
