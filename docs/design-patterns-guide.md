# Design Patterns Guide

This document provides an overview of several design patterns used in Java Spring Boot applications, along with code examples demonstrating their implementation.

## Repository Pattern
The Repository Pattern is used to separate the data access logic from the business logic. It provides a simple abstraction over the data layer.

### Example:
```java
@Repository
public interface UserRepository extends JpaRepository<User, Long> {
    Optional<User> findByUsername(String username);
}
```

## Service Pattern
The Service Pattern encapsulates the business logic of the application. It acts as a bridge between the controller and the data access layer.

### Example:
```java
@Service
public class UserService {
    @Autowired
    private UserRepository userRepository;

    public User findUserByUsername(String username) {
        return userRepository.findByUsername(username)
            .orElseThrow(() -> new ResourceNotFoundException("User not found"));
    }
}
```

## Strategy Pattern
The Strategy Pattern allows a method to be selected at runtime. It's useful in scenarios where multiple algorithms are applicable for a certain task.

### Example:
```java
public interface PaymentStrategy {
    void pay(int amount);
}

public class CreditCardPayment implements PaymentStrategy {
    public void pay(int amount) {
        System.out.println("Paid " + amount + " using Credit Card.");
    }
}

public class ShoppingCart {
    private PaymentStrategy paymentStrategy;

    public void setPaymentStrategy(PaymentStrategy paymentStrategy) {
        this.paymentStrategy = paymentStrategy;
    }

    public void checkout(int amount) {
        paymentStrategy.pay(amount);
    }
}
```

## Controller Layer
The controller layer handles incoming requests and returns responses.

### Example:
```java
@RestController
@RequestMapping("/users")
public class UserController {
    @Autowired
    private UserService userService;

    @GetMapping("/{username}")
    public ResponseEntity<User> getUser(@PathVariable String username) {
        User user = userService.findUserByUsername(username);
        return ResponseEntity.ok(user);
    }
}
```

## Data Transfer Object (DTO)
DTOs are used to transfer data between layers. They can help in reducing the number of calls needed to transfer data over a network.

### Example:
```java
public class UserDTO {
    private Long id;
    private String username;
    private String email;

    // Getters and Setters
}
```

## Exception Handling
Global exception handling can be done using @ControllerAdvice.

### Example:
```java
@ControllerAdvice
public class GlobalExceptionHandler {
    @ExceptionHandler(ResourceNotFoundException.class)
    public ResponseEntity<String> handleResourceNotFound(ResourceNotFoundException ex) {
        return new ResponseEntity<>(ex.getMessage(), HttpStatus.NOT_FOUND);
    }
}
```

## Unit Tests
Unit tests are crucial for verifying the smallest parts of an application in isolation.

### Example:
```java
@SpringBootTest
public class UserServiceTest {
    @MockBean
    private UserRepository userRepository;

    @Autowired
    private UserService userService;

    @Test
    public void testFindUserByUsername() {
        User user = new User();
        user.setUsername("testuser");
        Mockito.when(userRepository.findByUsername("testuser")).thenReturn(Optional.of(user));

        User found = userService.findUserByUsername("testuser");
        assertEquals(user.getUsername(), found.getUsername());
    }
}
```

## Integration Tests
Integration tests verify the behavior of the application components in a broader context.

### Example:
```java
@AutoConfigureMockMvc
@WebMvcTest(UserController.class)
public class UserControllerTest {
    @Autowired
    private MockMvc mockMvc;

    @Test
    public void testGetUser() throws Exception {
        mockMvc.perform(get("/users/testuser"))
            .andExpect(status().isOk());
    }
}
```

---
This is a high-level overview and can be expanded with additional patterns and more detailed explanations as needed.